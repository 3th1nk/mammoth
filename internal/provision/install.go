package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
)

// installTaskContext is the install flow's task context (schema documented
// here, versioned by field). The rendered answer files are snapshotted for
// offline audit and replay (docs/06-install-pipeline.md §2.2).
type installTaskContext struct {
	Token    string                `json:"token,omitempty"` // machine-facing credential
	Hostname string                `json:"hostname,omitempty"`
	Answers  []render.AnswerFile   `json:"answers,omitempty"`
	Boot     render.BootParams     `json:"boot,omitempty"`
	Resolved []render.ResolvedDisk `json:"resolved,omitempty"`
	Install  *installProgress      `json:"install,omitempty"`
}

type installProgress struct {
	BootedAt    *time.Time `json:"booted_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Status      string     `json:"status,omitempty"` // ok | failed
	Detail      string     `json:"detail,omitempty"`
}

// runInstallStage dispatches the install flow stages
// verify_layout → prepare_media → boot → install_os → verify_ready
// (docs/06-install-pipeline.md). Retry semantics per §6: every stage re-enters.
func (e *Executor) runInstallStage(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	stage := StageNames(FlowInstall)[seq]
	ctx = obs.With(ctx, obs.FieldStage, stage)
	switch stage {
	case "verify_layout":
		return e.verifyLayout(ctx, task, job, seq)
	case "prepare_media":
		return e.prepareMedia(ctx, task, job, seq)
	case "boot":
		return e.bootStage(ctx, task, job, seq)
	case "install_os":
		return e.installOSStage(ctx, task, job, seq)
	case "verify_ready":
		return e.verifyReady(ctx, task, job)
	default:
		return classifiedErr("JOB_UNKNOWN_STAGE", false, "unknown install stage %s", stage)
	}
}

// ── spec parsing (submit-side validated; exec-side re-reads the snapshot) ───

type installSpecView struct {
	Image struct {
		Source   string `json:"source"`
		ImageID  string `json:"image_id"`
		Checksum string `json:"checksum"`
		Distro   string `json:"distro"`
	} `json:"image"`
	Storage struct {
		Disks []storageDiskView `json:"disks"`
	} `json:"storage"`
	Identity struct {
		HostnamePattern string `json:"hostname_pattern"`
	} `json:"identity"`
	Access struct {
		RootPassword string   `json:"root_password"`
		SSHKeys      []string `json:"ssh_keys"`
	} `json:"access"`
	Network []networkView `json:"network"`
	Scripts []scriptView  `json:"scripts"`
}

// networkView mirrors the contract's network entries (docs/04-install-spec.md §5.2).
type networkView struct {
	Match *struct {
		MAC        string `json:"mac"`
		PCIAddress string `json:"pci_address"`
		Name       string `json:"name"`
	} `json:"match"`
	SetName string `json:"set_name"`
	Bond    *struct {
		Interfaces []struct {
			Match struct {
				MAC        string `json:"mac"`
				PCIAddress string `json:"pci_address"`
				Name       string `json:"name"`
			} `json:"match"`
			SetName string `json:"set_name"`
		} `json:"interfaces"`
		Mode   string                 `json:"mode"`
		Params map[string]interface{} `json:"params"`
	} `json:"bond"`
	VLAN *struct {
		ID   int    `json:"id"`
		Link string `json:"link"`
	} `json:"vlan"`
	Addresses []string `json:"addresses"`
	Routes    []struct {
		To  string `json:"to"`
		Via string `json:"via"`
	} `json:"routes"`
	Nameservers struct {
		Addresses []string `json:"addresses"`
		Search    []string `json:"search"`
	} `json:"nameservers"`
	MTU int `json:"mtu"`
}

func (n networkView) toRender() render.NetworkEntry {
	out := render.NetworkEntry{
		SetName: n.SetName,
		MTU:     n.MTU,
	}
	if n.Match != nil {
		out.Match = &render.NetMatch{MAC: n.Match.MAC, PCIAddress: n.Match.PCIAddress, Name: n.Match.Name}
	}
	if n.Bond != nil {
		var macs []string
		for _, i := range n.Bond.Interfaces {
			macs = append(macs, i.Match.MAC)
		}
		params := map[string]string{}
		for k, v := range n.Bond.Params {
			params[k] = fmt.Sprint(v) // numeric/bool params stringify (miimon 100 → "100")
		}
		out.Bond = &render.NetBond{SlavesMACs: macs, Mode: n.Bond.Mode, Params: params}
	}
	if n.VLAN != nil {
		out.VLAN = &render.NetVLAN{ID: n.VLAN.ID, Link: n.VLAN.Link}
	}
	out.Addresses = n.Addresses
	for _, r := range n.Routes {
		out.Routes = append(out.Routes, render.NetRoute{To: r.To, Via: r.Via})
	}
	out.Nameservers = n.Nameservers.Addresses
	out.Search = n.Nameservers.Search
	return out
}

type storageDiskView struct {
	Select struct {
		Match struct {
			Serial    string `json:"serial"`
			Type      string `json:"type"`
			Size      string `json:"size"`
			Protocol  string `json:"protocol"`
			Removable *bool  `json:"removable"`
		} `json:"match"`
	} `json:"select"`
	Wipe       bool            `json:"wipe"`
	Keep       string          `json:"keep"`
	Partitions []partitionView `json:"partitions"`
}

type partitionView struct {
	Size  string   `json:"size"`
	FS    string   `json:"fs"`
	Mount string   `json:"mount"`
	Flags []string `json:"flags"`
}

type scriptView struct {
	Stage         string `json:"stage"`
	ContentBase64 string `json:"content_base64"`
	URL           string `json:"url"`
	ExpectedExits []int  `json:"expected_exit_codes"`
}

// loadSpec resolves the effective spec: the per-task snapshot (base spec
// shallow-merged with the machine's override, taken at submit time) wins over
// the job-level snapshot (docs/04-install-spec.md §5 targets.overrides).
func (e *Executor) loadSpec(ctx context.Context, task *store.Task, job *store.Job) (*installSpecView, error) {
	var spec installSpecView
	source := job.SpecResolved
	if len(task.Context) > 0 {
		var probe struct {
			Spec json.RawMessage `json:"spec"`
		}
		if json.Unmarshal(task.Context, &probe) == nil && len(probe.Spec) > 0 {
			source = probe.Spec
		}
	}
	if len(source) == 0 {
		return nil, classifiedErr("SCHEMA_INVALID_SPEC", false, "install job without a spec snapshot")
	}
	if err := json.Unmarshal(source, &spec); err != nil {
		return nil, classifiedErr("SCHEMA_INVALID_SPEC", false, "spec snapshot unreadable: %s", err.Error())
	}
	if spec.Image.Distro == "" {
		return nil, classifiedErr("SCHEMA_INVALID_SPEC", false, "spec image.distro missing")
	}
	return &spec, nil
}

// ── stage 1: verify_layout (docs/06-install-pipeline.md §1) ─────────────────

func (e *Executor) verifyLayout(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	spec, err := e.loadSpec(ctx, task, job)
	if err != nil {
		return err
	}
	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}
	var hw bmc.HardwareView
	if len(m.Hardware) > 0 {
		if err := json.Unmarshal(m.Hardware, &hw); err != nil {
			return classifiedErr("SCHEMA_INVALID_HARDWARE", false, "machine hardware view unreadable")
		}
	}
	if len(hw.Disks) == 0 {
		return classifiedErr("LAYOUT_DISK_NOT_FOUND", false,
			"machine %s has no hardware inventory; run discover first", task.MachineID)
	}

	resolved := make([]render.ResolvedDisk, 0, len(spec.Storage.Disks))
	for i, d := range spec.Storage.Disks {
		if d.Keep != "" {
			// keep semantics arrive with M4 (%pre drift machinery); rejected
			// explicitly rather than half-supported.
			return classifiedErr("SCHEMA_INVALID_STORAGE", false,
				"keep semantics arrive with M4; M3 supports the wipe path only")
		}
		match := d.Select.Match
		// Selector fields combine (docs/04-install-spec.md §5.1): exact
		// fields filter first; `size` then picks the largest/smallest
		// WITHIN the filtered pool (e.g. type=nvme + size=largest means
		// the largest NVMe, not the largest disk overall).
		var pool []bmc.DiskView
		for _, hd := range hw.Disks {
			if match.Serial != "" && hd.Serial != match.Serial {
				continue
			}
			if match.Type != "" && !diskTypeMatches(match.Type, hd) {
				continue
			}
			if match.Protocol != "" && !strings.EqualFold(match.Protocol, hd.Protocol) {
				continue
			}
			if match.Removable != nil && hd.Removable != *match.Removable {
				continue
			}
			pool = append(pool, hd)
		}
		var candidates []bmc.DiskView
		if match.Size != "" && len(pool) > 0 {
			candidates = append(candidates, *bestDiskBySize(pool, match.Size))
		} else {
			candidates = pool
		}
		switch len(candidates) {
		case 0:
			return classifiedErr("LAYOUT_DISK_NOT_FOUND", false,
				"storage.disks[%d]: selector matches no disk on %s", i, task.MachineID)
		case 1:
		default:
			return classifiedErr("SELECT_AMBIGUOUS", false,
				"storage.disks[%d]: selector matches %d disks; refine the match", i, len(candidates))
		}
		rd := render.ResolvedDisk{Device: candidates[0].Name, Serial: candidates[0].Serial, Wipe: d.Wipe}
		if !d.Wipe {
			return classifiedErr("SCHEMA_INVALID_STORAGE", false,
				"storage.disks[%d]: declare wipe (keep arrives with M4)", i)
		}
		for _, p := range d.Partitions {
			rp := render.ResolvedPartition{Mount: p.Mount, FS: p.FS, Flags: p.Flags}
			if p.Size == "rest" {
				rp.Grow = true
			} else {
				mb, err := sizeToMB(p.Size)
				if err != nil {
					return classifiedErr("SCHEMA_INVALID_STORAGE", false,
						"storage.disks[%d].partitions[%d]: %s", i, len(rp.Mount), err.Error())
				}
				rp.SizeMB = mb
			}
			rd.Partitions = append(rd.Partitions, rp)
		}
		resolved = append(resolved, rd)
	}

	ictx := installTaskContext{}
	_ = json.Unmarshal(task.Context, &ictx)
	raw, _ := json.Marshal(resolved)
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{"resolved": json.RawMessage(raw)}); err != nil {
		return err
	}
	ictx.Resolved = resolved
	obs.FromContext(ctx).InfoContext(ctx, "layout resolved",
		"disks", len(resolved))
	return nil
}

func diskTypeMatches(want string, d bmc.DiskView) bool {
	switch want {
	case "nvme":
		return strings.EqualFold(d.Protocol, "nvme")
	case "ssd":
		return d.Medium == "ssd"
	case "hdd":
		return d.Medium == "hdd"
	default:
		return false
	}
}

func bestDiskBySize(disks []bmc.DiskView, which string) *bmc.DiskView {
	var best *bmc.DiskView
	for i := range disks {
		d := &disks[i]
		if best == nil || (which == "largest" && d.SizeBytes > best.SizeBytes) ||
			(which == "smallest" && d.SizeBytes < best.SizeBytes) {
			best = d
		}
	}
	return best
}

// sizeToMB parses the size syntax ("512M", "100G") into megabytes.
func sizeToMB(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	unit := s[len(s)-1]
	num := s[:len(s)-1]
	var mult int
	switch unit {
	case 'G':
		mult, num = 1024, s[:len(s)-1]
	case 'M':
		mult, num = 1, s[:len(s)-1]
	case 'T':
		mult, num = 1024*1024, s[:len(s)-1]
	default:
		// bare number = megabytes
		mult, num = 1, s
	}
	var n int
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// ── stage 2: prepare_media (docs/06-install-pipeline.md §2) ─────────────────

func (e *Executor) prepareMedia(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	// Context mutates between stages: always re-enter from the fresh record.
	fresh, ferr := e.Jobs.GetTask(ctx, task.ID)
	if ferr != nil {
		return ferr
	}
	task = fresh
	spec, err := e.loadSpec(ctx, task, job)
	if err != nil {
		return err
	}
	var ictx installTaskContext
	if err := json.Unmarshal(task.Context, &ictx); err != nil {
		return classifiedErr("JOB_CONTEXT_CORRUPT", false, "task context unreadable")
	}
	if ictx.Token == "" {
		return classifiedErr("JOB_CONTEXT_CORRUPT", false, "task token missing (install jobs only)")
	}
	if len(ictx.Resolved) == 0 {
		return classifiedErr("JOB_STAGE_ORDER", false, "layout not resolved; verify_layout must run first")
	}

	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}

	// Access: per-task random root password delivered once via the task
	// event (docs/04-install-spec.md §6); explicit values pass through.
	rootPassword := spec.Access.RootPassword
	if rootPassword == "generate" {
		rootPassword, err = randomPassword()
		if err != nil {
			return err
		}
		e.Events.Append(ctx, "task", task.ID, "task.root_password", map[string]any{
			"password": rootPassword, "note": "one-time; ks.cfg is the only persistent copy",
		})
	}

	var hw bmc.HardwareView
	_ = json.Unmarshal(m.Hardware, &hw)
	bootDrive := ictx.Resolved[0].Device
	for _, d := range ictx.Resolved {
		for _, p := range d.Partitions {
			if p.Mount == "/" {
				bootDrive = d.Device
			}
		}
	}

	in := render.InstallInputs{
		TaskToken:     ictx.Token,
		MachineID:     m.ID,
		Hostname:      ictx.Hostname,
		ImageSource:   spec.Image.Source,
		RootPassword:  rootPassword,
		SSHPublicKeys: spec.Access.SSHKeys,
		BootDrive:     bootDrive,
		Disks:         ictx.Resolved,
		Network:       networkEntries(spec.Network),
		AnswerURL:     fmt.Sprintf("%s/render/%s/ks.cfg", strings.TrimSuffix(e.ExternalURL, "/"), ictx.Token),
		CompleteURL:   fmt.Sprintf("%s/render/%s/complete", strings.TrimSuffix(e.ExternalURL, "/"), ictx.Token),
		Scripts:       scriptsFrom(spec.Scripts),
	}
	driver, err := e.Render.For(spec.Image.Distro)
	if err != nil {
		return classifiedErr("SCHEMA_UNKNOWN_DISTRO", false, "%s", err.Error())
	}
	answers, boot, err := driver.RenderAnswers(in, render.MachineView{
		ID: m.ID, Hostname: ictx.Hostname, Hardware: &hw,
	})
	if err != nil {
		return classifiedErr("RENDER_FAILED", false, "%s", err.Error())
	}

	raw, _ := json.Marshal(ictx.Install)
	patch := map[string]any{
		"answers": answers,
		"boot":    boot,
	}
	if raw != nil && string(raw) != "null" {
		patch["install"] = json.RawMessage(raw)
	}
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, patch); err != nil {
		return err
	}
	obs.FromContext(ctx).InfoContext(ctx, "answers rendered",
		"files", len(answers), "distro", spec.Image.Distro)
	return nil
}

func networkEntries(spec []networkView) []render.NetworkEntry {
	out := make([]render.NetworkEntry, 0, len(spec))
	for _, n := range spec {
		out = append(out, n.toRender())
	}
	return out
}

func scriptsFrom(spec []scriptView) []render.ScriptEntry {
	out := make([]render.ScriptEntry, 0, len(spec))
	for _, s := range spec {
		decoded := ""
		if s.ContentBase64 != "" {
			if b, err := base64.StdEncoding.DecodeString(s.ContentBase64); err == nil {
				decoded = string(b)
			}
		}
		out = append(out, render.ScriptEntry{
			Stage: s.Stage, Inline: decoded, URL: s.URL, ExpectedExit: s.ExpectedExits,
		})
	}
	return out
}

func randomPassword() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// ── stage 3: boot (docs/06-install-pipeline.md §3) ──────────────────────────

func (e *Executor) bootStage(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	fresh, ferr := e.Jobs.GetTask(ctx, task.ID)
	if ferr != nil {
		return ferr
	}
	task = fresh
	var ictx installTaskContext
	if err := json.Unmarshal(task.Context, &ictx); err != nil {
		return classifiedErr("JOB_CONTEXT_CORRUPT", false, "task context unreadable")
	}
	if len(ictx.Answers) == 0 {
		return classifiedErr("JOB_STAGE_ORDER", false, "answers not rendered; prepare_media must run first")
	}
	spec, err := e.loadSpec(ctx, task, job)
	if err != nil {
		return err
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	distroISO := bmc.MediaImage{URL: spec.Image.Source, Kind: bmc.MediaDistro}
	bootMedia := bmc.MediaImage{URL: ictx.Boot.AnswerURL, Kind: bmc.MediaBoot}

	if _, err := e.BMC.Do(ctx, addr, cred, proto, "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.MountMedia(ctx, addr, cred, distroISO)
	}); err != nil {
		return err
	}
	// Media B: the task boot media carrying inst.ks (grub.cfg served by
	// Mammoth; ISO assembly is the builder-container step).
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.MountMedia(ctx, addr, cred, bootMedia)
	}); err != nil {
		return err
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootCDROM, true)
	}); err != nil {
		return err
	}
	// Power: cycle if on, otherwise plain power-on.
	ps, err := e.BMC.Do(ctx, addr, cred, proto, "power_state", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.PowerState(ctx, addr, cred)
	})
	action := bmc.PowerOn
	if err == nil && ps.(bmc.PowerState) == bmc.PowerStateOn {
		action = bmc.Cycle
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetPower(ctx, addr, cred, action)
	}); err != nil {
		return err
	}

	now := time.Now().UTC()
	if err := e.Jobs.RecordInstallProgress(ctx, task.ID, map[string]any{"booted_at": now}); err != nil {
		return err
	}
	obs.FromContext(ctx).InfoContext(ctx, "machine booted into installer",
		"action", string(action))
	return nil
}

// ── stage 4: install_os (wait for the machine's completion report) ──────────

func (e *Executor) installOSStage(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	var ictx installTaskContext
	if err := json.Unmarshal(task.Context, &ictx); err != nil {
		return classifiedErr("JOB_CONTEXT_CORRUPT", false, "task context unreadable")
	}
	// Media B comes out as soon as the installer is up — second boots would
	// otherwise re-enter the boot media (docs/06-install-pipeline.md §3.4).
	if ictx.Token != "" && len(ictx.Answers) > 0 {
		if cred, addr, proto, ok := e.outOfBand(ctx, task); ok {
			bootMedia := bmc.MediaImage{URL: ictx.Boot.AnswerURL, Kind: bmc.MediaBoot}
			if _, err := e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
				return nil, d.EjectMedia(ctx, addr, cred, bootMedia)
			}); err != nil {
				obs.FromContext(ctx).WarnContext(ctx, "eject boot media failed (continuing)", "err", err.Error())
			}
		}
	}

	deadline := time.Now().Add(job.Policy.TaskTimeout())
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		fresh, err := e.Jobs.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		if cancelReq, _ := e.Jobs.IsCancelRequested(ctx, task.ID); cancelReq {
			return ErrCanceled
		}
		var ictx2 installTaskContext
		_ = json.Unmarshal(fresh.Context, &ictx2)
		if ictx2.Install != nil && ictx2.Install.CompletedAt != nil {
			if ictx2.Install.Status == "failed" {
				return classifiedErr("INSTALL_FAILED", true, "installer reported failure: %s", ictx2.Install.Detail)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return classifiedErr("INSTALL_TIMEOUT", true,
				"install did not report completion within %s", job.Policy.TaskTimeout())
		}
	}
}

// ── stage 5: verify_ready (docs/06-install-pipeline.md §4) ──────────────────

func (e *Executor) verifyReady(ctx context.Context, task *store.Task, job *store.Job) error {
	var ictx installTaskContext
	if err := json.Unmarshal(task.Context, &ictx); err != nil {
		return classifiedErr("JOB_CONTEXT_CORRUPT", false, "task context unreadable")
	}
	if ictx.Install == nil || ictx.Install.Status != "ok" {
		return classifiedErr("INSTALL_NOT_VERIFIED", true, "no successful completion report recorded")
	}

	// RT consistency: when in-band access is configured, confirm the new
	// system answers (docs/06-install-pipeline.md §4). Without in-band
	// credentials the completion report is the verification surface.
	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}
	if m.SSHCredentialID != nil && m.SSHAddress != "" {
		credRow, err := e.Credentials.Get(ctx, *m.SSHCredentialID)
		if err == nil && credRow.Type == "ssh" {
			plain, derr := e.Crypto.Decrypt(credRow.SecretEncrypted)
			if derr == nil {
				var secret struct {
					Username string `json:"username"`
					Password string `json:"password"`
				}
				if json.Unmarshal(plain, &secret) == nil {
					probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					if _, err := e.Inband.Collect(probeCtx, m.SSHAddress, inbandssh.Credentials{
						Username: secret.Username, Password: secret.Password,
					}); err != nil {
						return classifiedErr("INSTALL_NOT_REACHABLE", true,
							"new system did not answer in-band: %s", err.Error())
					}
				}
			}
		}
	}

	// Success compensation: eject all media, restore boot order.
	if cred, addr, proto, ok := e.outOfBand(ctx, task); ok {
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootDisk, false)
		}); err != nil {
			obs.FromContext(ctx).WarnContext(ctx, "restore boot order failed", "err", err.Error())
		}
	}
	obs.FromContext(ctx).InfoContext(ctx, "install verified ready")
	return nil
}
