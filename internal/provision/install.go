package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/builder"
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
)

// installTaskContext is the install flow's task context (schema documented
// here, versioned by field). The rendered answer files are snapshotted for
// offline audit and replay (docs/06-install-pipeline.md §2.2).
type installTaskContext struct {
	Token    string              `json:"token,omitempty"` // machine-facing credential
	Hostname string              `json:"hostname,omitempty"`
	Answers  []render.AnswerFile `json:"answers,omitempty"`
	Boot     render.BootParams   `json:"boot,omitempty"`
	// MediaURI is the BMC-accessible URI of the assembled boot ISO
	// (nfs://host/export/boot-<token>.iso for NFS-served media repos).
	MediaURI string `json:"media_uri,omitempty"`
	Resolved struct {
		Disks []render.ResolvedDisk `json:"disks"`
		Raid  []render.ResolvedRaid `json:"raid,omitempty"`
	} `json:"resolved,omitempty"`
	RaidBindings map[string]string `json:"raid_bindings,omitempty"`
	Install      *installProgress  `json:"install,omitempty"`
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
	case "configure_raid":
		return e.configureRaid(ctx, task, job, seq)
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
		Raid  []raidSpecView    `json:"raid"`
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
	Preserve   []preserveView  `json:"preserve"`
	Partitions []partitionView `json:"partitions"`
}

type raidSpecView struct {
	Name    string `json:"name"`
	Level   string `json:"level"`
	Mode    string `json:"mode"`
	Members []struct {
		Match struct {
			Serial    string `json:"serial"`
			Type      string `json:"type"`
			Size      string `json:"size"`
			Protocol  string `json:"protocol"`
			Removable *bool  `json:"removable"`
		} `json:"match"`
	} `json:"members"`
	Partitions []partitionView `json:"partitions"`
}

type preserveView struct {
	Number int    `json:"number"`
	Mount  string `json:"mount"`
	FS     string `json:"fs"`
}

type partitionView struct {
	Size     string   `json:"size"`
	FS       string   `json:"fs"`
	Mount    string   `json:"mount"`
	Flags    []string `json:"flags"`
	Number   int      `json:"number"`
	Preserve bool     `json:"preserve"`
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
		// Redfish-blind storage (docs/compat/huawei.md §4): fall back to the
		// in-band layout snapshot, which carries device names + serials —
		// enough to resolve selectors on machines whose BMC hides drives.
		snap, serr := e.layoutSnapshot(ctx, task.MachineID)
		if serr != nil {
			return classifiedErr("LAYOUT_DISK_NOT_FOUND", false,
				"machine %s has no hardware inventory and no layout snapshot: run discover (with in-band access)",
				task.MachineID)
		}
		for _, sd := range snap {
			serial := sd.Match["serial"]
			hw.Disks = append(hw.Disks, bmc.DiskView{
				Name: sd.Device, Serial: serial, SizeBytes: snapshotDiskSize(sd),
				Protocol: "inband",
			})
		}
	}

	resolved := make([]render.ResolvedDisk, 0, len(spec.Storage.Disks))
	used := map[string]int{} // device → spec disk index (overlap detection)
	for i, d := range spec.Storage.Disks {
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
		device := candidates[0].Name
		if prev, dup := used[device]; dup {
			return classifiedErr("SCHEMA_INVALID_STORAGE", false,
				"storage.disks[%d] resolves to %s, already claimed by disks[%d] (wipe/keep overlap)", i, device, prev)
		}
		used[device] = i
		rd := render.ResolvedDisk{Device: device, Serial: candidates[0].Serial}

		switch {
		case d.Keep == "disk":
			// keep: disk — the disk is never touched (docs/04-install-spec.md §5.1).
			rd.KeepDisk = true
		case d.Keep == "partitions":
			// Bind the latest snapshot: preserved partitions must exist; the
			// snapshot facts become the %pre drift baseline (docs/06 §4).
			snap, err := e.layoutSnapshot(ctx, task.MachineID)
			if err != nil {
				return classifiedErr("LAYOUT_SNAPSHOT_REQUIRED", false,
					"storage.disks[%d]: keep:partitions requires a layout snapshot (%s)", i, err.Error())
			}
			var snapDisk *snapPartition
			for di := range snap {
				if snap[di].Match["serial"] == candidates[0].Serial || snap[di].Device == device {
					snapDisk = &snap[di]
					break
				}
			}
			if snapDisk == nil {
				return classifiedErr("LAYOUT_SNAPSHOT_REQUIRED", false,
					"storage.disks[%d]: disk %s absent from the machine's snapshot", i, device)
			}
			preserveNums := map[int]bool{}
			for _, p := range d.Preserve {
				preserveNums[p.Number] = true
			}
			for _, sp := range snapDisk.Partitions {
				onPart := onPartName(device, sp.Number)
				rd.Baseline = append(rd.Baseline, render.BaselinePartition{
					Device: onPart, Number: sp.Number,
					StartBytes: sp.StartBytes, EndBytes: sp.EndBytes, SizeBytes: sp.SizeBytes,
					UUID: sp.UUID, FSType: sp.FSType, Mountpoint: sp.Mountpoint,
				})
				if !preserveNums[sp.Number] {
					rd.Remove = append(rd.Remove, onPart)
				}
			}
			for _, p := range d.Partitions {
				rp := render.ResolvedPartition{Mount: p.Mount, FS: p.FS, Flags: p.Flags}
				if p.Size == "rest" {
					rp.Grow = true
				} else {
					mb, err := sizeToMB(p.Size)
					if err != nil {
						return classifiedErr("SCHEMA_INVALID_STORAGE", false,
							"storage.disks[%d].partitions: %s", i, err.Error())
					}
					rp.SizeMB = mb
				}
				rd.Partitions = append(rd.Partitions, rp)
			}
			// preserve[] entries are the kept mounts: each becomes a reused
			// partition (unformatted, original UUID — docs/04 §5.1 ③).
			for _, pv := range d.Preserve {
				var base *render.BaselinePartition
				for bi := range rd.Baseline {
					if rd.Baseline[bi].Number == pv.Number {
						base = &rd.Baseline[bi]
					}
				}
				if base == nil {
					return classifiedErr("SCHEMA_INVALID_STORAGE", false,
						"storage.disks[%d]: preserve %d not in snapshot", i, pv.Number)
				}
				rd.Partitions = append(rd.Partitions, render.ResolvedPartition{
					Mount:    firstNonEmptyStr(pv.Mount, base.Mountpoint),
					FS:       firstNonEmptyStr(pv.FS, base.FSType),
					Preserve: true,
					Number:   pv.Number,
					OnPart:   onPartName(device, pv.Number),
					UUID:     base.UUID,
				})
			}
		default:
			if !d.Wipe {
				return classifiedErr("SCHEMA_INVALID_STORAGE", false,
					"storage.disks[%d]: declare wipe or keep", i)
			}
			rd.Wipe = true
			for _, p := range d.Partitions {
				rp := render.ResolvedPartition{Mount: p.Mount, FS: p.FS, Flags: p.Flags}
				if p.Size == "rest" {
					rp.Grow = true
				} else {
					mb, err := sizeToMB(p.Size)
					if err != nil {
						return classifiedErr("SCHEMA_INVALID_STORAGE", false,
							"storage.disks[%d].partitions: %s", i, err.Error())
					}
					rp.SizeMB = mb
				}
				rd.Partitions = append(rd.Partitions, rp)
			}
		}
		resolved = append(resolved, rd)
	}

	// RAID resolution (docs/09-roadmap.md M6): members resolve against the
	// same hardware pool; overlap with disks[] entries is rejected — a RAID
	// member is consumed by the volume.
	usedDevices := map[string]string{}
	for _, rd := range resolved {
		usedDevices[rd.Device] = "disks"
	}
	raids := make([]render.ResolvedRaid, 0, len(spec.Storage.Raid))
	for i, r := range spec.Storage.Raid {
		mode := r.Mode
		if mode == "" {
			mode = "software"
		}
		if mode != "software" && mode != "hardware" {
			return classifiedErr("SCHEMA_INVALID_STORAGE", false, "raid[%d]: unknown mode %s", i, mode)
		}
		if err := checkRaidShape(i, r.Level, len(r.Members)); err != nil {
			return err
		}
		members := make([]string, 0, len(r.Members))
		for j, m := range r.Members {
			var pool []bmc.DiskView
			for _, hd := range hw.Disks {
				if m.Match.Serial != "" && hd.Serial != m.Match.Serial {
					continue
				}
				if m.Match.Type != "" && !diskTypeMatches(m.Match.Type, hd) {
					continue
				}
				if m.Match.Protocol != "" && !strings.EqualFold(m.Match.Protocol, hd.Protocol) {
					continue
				}
				if m.Match.Removable != nil && hd.Removable != *m.Match.Removable {
					continue
				}
				if m.Match.Size != "" {
					best := bestDiskBySize(hw.Disks, m.Match.Size)
					if best == nil || hd.Name != best.Name {
						continue
					}
				}
				pool = append(pool, hd)
			}
			if len(pool) != 1 {
				return classifiedErr("LAYOUT_DISK_NOT_FOUND", false,
					"raid[%d].members[%d]: selector matches %d disks (need exactly 1)", i, j, len(pool))
			}
			if prev, dup := usedDevices[pool[0].Name]; dup {
				return classifiedErr("SCHEMA_INVALID_STORAGE", false,
					"raid[%d].members[%d]: %s already claimed by %s", i, j, pool[0].Name, prev)
			}
			usedDevices[pool[0].Name] = fmt.Sprintf("raid[%d]", i)
			members = append(members, pool[0].Name)
		}
		rr := render.ResolvedRaid{Name: r.Name, Level: r.Level, Mode: mode, Members: members}
		for _, p := range r.Partitions {
			rp := render.ResolvedPartition{Mount: p.Mount, FS: p.FS, Flags: p.Flags}
			if p.Size == "rest" {
				rp.Grow = true
			} else {
				mb, err := sizeToMB(p.Size)
				if err != nil {
					return classifiedErr("SCHEMA_INVALID_STORAGE", false,
						"raid[%d].partitions: %s", i, err.Error())
				}
				rp.SizeMB = mb
			}
			rr.Partitions = append(rr.Partitions, rp)
		}
		raids = append(raids, rr)
	}

	ictx := installTaskContext{}
	_ = json.Unmarshal(task.Context, &ictx)
	raw, _ := json.Marshal(struct {
		Disks []render.ResolvedDisk `json:"disks"`
		Raid  []render.ResolvedRaid `json:"raid,omitempty"`
	}{Disks: resolved, Raid: raids})
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{"resolved": json.RawMessage(raw)}); err != nil {
		return err
	}
	ictx.Resolved.Disks = resolved
	ictx.Resolved.Raid = raids
	obs.FromContext(ctx).InfoContext(ctx, "layout resolved",
		"disks", len(resolved), "raid", len(raids))
	return nil
}

// checkRaidShape enforces level/member-count rules (docs: 0→2+, 1→2+, 5→3+, 10→4+).
func checkRaidShape(i int, level string, members int) error {
	var min, max int
	switch level {
	case "0":
		min, max = 2, 0
	case "1":
		min, max = 2, 4
	case "5":
		min, max = 3, 0
	case "10":
		min, max = 4, 0
		if members%2 != 0 {
			return classifiedErr("SCHEMA_INVALID_STORAGE", false,
				"raid[%d]: level 10 requires an even member count", i)
		}
	default:
		return classifiedErr("SCHEMA_INVALID_STORAGE", false, "raid[%d]: unknown level %q", i, level)
	}
	if members < min || (max > 0 && members > max) {
		return classifiedErr("SCHEMA_INVALID_STORAGE", false,
			"raid[%d]: level %s needs %d+ members (got %d)", i, level, min, members)
	}
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
	if len(ictx.Resolved.Disks) == 0 && len(ictx.Resolved.Raid) == 0 {
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
	// Bootloader target: the disk (or bound hardware-raid volume) hosting /.
	bootDrive := ""
	for _, d := range ictx.Resolved.Disks {
		for _, p := range d.Partitions {
			if p.Mount == "/" {
				bootDrive = d.Device
			}
		}
	}
	for _, r := range ictx.Resolved.Raid {
		if r.BoundDevice == "" {
			continue
		}
		for _, p := range r.Partitions {
			if p.Mount == "/" {
				bootDrive = r.BoundDevice
			}
		}
		if bootDrive == "" {
			bootDrive = r.BoundDevice
		}
	}

	in := render.InstallInputs{
		TaskToken:     ictx.Token,
		DriftCheck:    job.Policy.VerifyLayout == nil || *job.Policy.VerifyLayout,
		MachineID:     m.ID,
		Hostname:      ictx.Hostname,
		ImageSource:   spec.Image.Source,
		RootPassword:  rootPassword,
		SSHPublicKeys: spec.Access.SSHKeys,
		BootDrive:     bootDrive,
		Disks:         ictx.Resolved.Disks,
		Network:       networkEntries(spec.Network),
		AnswerBaseURL: fmt.Sprintf("%s/render/%s", strings.TrimSuffix(e.ExternalURL, "/"), ictx.Token),
		CompleteURL:   fmt.Sprintf("%s/render/%s/complete", strings.TrimSuffix(e.ExternalURL, "/"), ictx.Token),
		Scripts:       scriptsFrom(spec.Scripts),
	}
	raidInputs := ictx.Resolved.Raid
	for i := range raidInputs {
		if raidInputs[i].Mode == "hardware" && raidInputs[i].BoundDevice == "" {
			raidInputs[i].BoundDevice = ictx.RaidBindings[raidInputs[i].Name]
		}
	}
	in.Raid = raidInputs

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

	// Assemble the per-task boot media (media B): bootloader carries
	// inst.ks=<token URL>; packages come from inst.repo over the network
	// (docs/06-install-pipeline.md §2.1). The ISO lands in the media repo
	// and is exposed to the BMC through the NFS base URI.
	mediaFile := fmt.Sprintf("boot-%s.iso", ictx.Token)
	mediaURI := e.mediaURIFor(filepath.Base(mediaFile))
	distroISO, err := e.Builder.EnsureISO(ctx, spec.Image.Source, e.MediaDir)
	if err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"distro ISO fetch failed: %s", err.Error())
	}
	if _, err := e.Builder.BuildBootISO(ctx, builder.BootMediaOptions{
		ISOPath:    distroISO,
		OutputPath: filepath.Join(e.MediaDir, filepath.Base(mediaFile)),
		Timeout:    10 * time.Minute,
	}, boot.KernelArgs); err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"boot media build failed: %s", err.Error())
	}

	raw, _ := json.Marshal(ictx.Install)
	patch := map[string]any{
		"answers":   answers,
		"boot":      boot,
		"media_uri": mediaURI,
	}
	if raw != nil && string(raw) != "null" {
		patch["install"] = json.RawMessage(raw)
	}
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, patch); err != nil {
		return err
	}
	obs.FromContext(ctx).InfoContext(ctx, "answers rendered, boot media built",
		"files", len(answers), "distro", spec.Image.Distro, "media_uri", mediaURI)
	return nil
}

// distroTreeURL converts a distro ISO's source into the HTTP tree the boot
// files can be fetched from (the mirror serving the ISO also serves its
// extracted layout; see MAMMOTH_BOOT_TREE_BASE for overrides).
func distroTreeURL(source string) string {
	// Strip the .iso suffix: .../Rocky-9.7.iso → .../Rocky-9.7 (tree layout).
	return strings.TrimSuffix(source, ".iso")
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

// ── snapshot binding helpers (M4) ───────────────────────────────────────────

type snapPartition struct {
	Device     string            `json:"device"`
	Match      map[string]string `json:"match"`
	Partitions []snapPartitionEx `json:"partitions"`
}

type snapPartitionEx struct {
	Number     int    `json:"number"`
	StartBytes int64  `json:"start_bytes"`
	EndBytes   int64  `json:"end_bytes"`
	SizeBytes  int64  `json:"size_bytes"`
	FSType     string `json:"fstype"`
	UUID       string `json:"uuid"`
	Mountpoint string `json:"mountpoint"`
}

// layoutSnapshot loads and parses the machine's latest layout snapshot.
func (e *Executor) layoutSnapshot(ctx context.Context, machineID string) ([]snapPartition, error) {
	content, _, err := e.Machines.LatestLayout(ctx, machineID)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Disks []snapPartition `json:"disks"`
	}
	if err := json.Unmarshal(content, &wrapped); err != nil {
		return nil, fmt.Errorf("snapshot unreadable: %w", err)
	}
	snap := wrapped.Disks
	if len(snap) == 0 {
		return nil, fmt.Errorf("snapshot has no disks")
	}
	return snap, nil
}

// onPartName derives the full kernel partition name: nvme0n1 → nvme0n1p2,
// sda → sda2 (digit-suffixed parents take the 'p' separator).
func onPartName(parent string, number int) string {
	if len(parent) > 0 && parent[len(parent)-1] >= '0' && parent[len(parent)-1] <= '9' {
		return fmt.Sprintf("%sp%d", parent, number)
	}
	return fmt.Sprintf("%s%d", parent, number)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── configure_raid (docs/09-roadmap.md M6) ──────────────────────────────────

// configureRaid realizes hardware RAID volumes via the controller
// (Redfish Volume creation, or the fake equivalent). Software RAID needs no
// BMC action — anaconda builds md arrays at install time. Idempotent by
// volume name: existing volumes with the declared name are left alone
// (docs/06-install-pipeline.md §6 retry semantics extended to this stage).
func (e *Executor) configureRaid(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	spec, err := e.loadSpec(ctx, task, job)
	if err != nil {
		return err
	}
	hardware := false
	for _, r := range spec.Storage.Raid {
		if r.Mode == "hardware" {
			hardware = true
		}
	}
	if !hardware {
		// software-only (or no raid): nothing to do at BMC level.
		return nil
	}

	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}
	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}

	// before/after inventory diff binds each new logical drive to its
	// declared volume name.
	before := map[string]bool{}
	if len(m.Hardware) > 0 {
		var hw bmc.HardwareView
		if json.Unmarshal(m.Hardware, &hw) == nil {
			for _, d := range hw.Disks {
				before[d.Name] = true
			}
		}
	}

	bindings := map[string]string{}
	for _, r := range spec.Storage.Raid {
		if r.Mode != "hardware" {
			continue
		}
		res, cerr := e.BMC.Do(ctx, addr, cred, proto, "create_volume", func(ctx context.Context, d bmc.Driver) (any, error) {
			creator, okc := d.(bmc.VolumeCreator)
			if !okc {
				return nil, classifiedErr("BMC_UNSUPPORTED", false,
					"driver %s does not support volume creation", d.Name())
			}
			return nil, creator.CreateVolume(ctx, addr, cred, bmc.VolumeSpec{
				Name: r.Name, RAIDType: "RAID" + r.Level,
			})
		})
		if cerr != nil {
			var be *bmc.Error
			if errors.As(cerr, &be) && be.Kind == bmc.KindUnsupported {
				return classifiedErr("BMC_UNSUPPORTED", false, "raid volume creation: %s", cerr.Error())
			}
			// a volume that already exists from a previous attempt is fine
			if !strings.Contains(cerr.Error(), "already exists") {
				return cerr
			}
		}
		_ = res
		bindings[r.Name] = "" // bound below by re-inventory
	}

	// Re-inventory: the controller exposes the new logical drive(s).
	inv, ierr := e.BMC.Do(ctx, addr, cred, proto, "collect_inventory", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.CollectInventory(ctx, addr, cred)
	})
	if ierr != nil {
		return classifiedErr("INVENTORY_REFRESH_FAILED", true, "post-raid inventory: %s", ierr.Error())
	}
	hw := inv.(bmc.HardwareView)
	for _, r := range spec.Storage.Raid {
		if r.Mode != "hardware" {
			continue
		}
		for _, d := range hw.Disks {
			// fake/Redfish expose the volume under its declared name; bind
			// only drives that were not present before the stage.
			if d.Name == r.Name && !before[d.Name] {
				bindings[r.Name] = d.Name
			}
		}
		if bindings[r.Name] == "" {
			return classifiedErr("INVENTORY_REFRESH_FAILED", true,
				"raid volume %q did not appear in the refreshed inventory", r.Name)
		}
	}

	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{"raid_bindings": bindings}); err != nil {
		return err
	}
	obs.FromContext(ctx).InfoContext(ctx, "hardware raid configured",
		"volumes", len(bindings))
	return nil
}

// snapshotDiskSize computes a snapshot disk's total size from its partitions
// (end of the last partition, assuming contiguity — enough for selector
// size=largest/smallest ordering).
func snapshotDiskSize(sd snapPartition) int64 {
	var last int64
	for _, p := range sd.Partitions {
		if p.EndBytes > last {
			last = p.EndBytes
		}
	}
	return last
}

// mediaURIFor returns the BMC-accessible URI for a media file name.
func (e *Executor) mediaURIFor(filename string) string {
	if e.MediaNFSBase == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", strings.TrimSuffix(e.MediaNFSBase, "/"), filename)
}
