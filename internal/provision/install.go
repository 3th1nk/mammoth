package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// mediaProduced stages: once prepare_media has run, a boot ISO exists in the
// media export and every failed attempt must not strand it there.
func installMediaProduced(stage string) bool {
	switch stage {
	case "boot", "install_os", "verify_ready":
		return true
	}
	return false
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
		RootPassword string   `json:"root_password"` // absent/empty → generate
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
		rd := render.ResolvedDisk{Device: device, Serial: candidates[0].Serial, SizeBytes: candidates[0].SizeBytes}

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
		// Hardware RAID members select from the controller's PHYSICAL drives,
		// not the OS presentation (volume-first hides them —
		// docs/05-inventory.md §2). The enumerator capability supplies them;
		// without it, fall back to the inventory disks (fake/legacy BMCs).
		memberPool := hw.Disks
		if mode == "hardware" {
			if drives, err := e.physicalDrives(ctx, task); err == nil && len(drives) > 0 {
				memberPool = drives
			}
		}
		members := make([]string, 0, len(r.Members))
		memberSerials := make([]string, 0, len(r.Members))
		for j, m := range r.Members {
			var pool []bmc.DiskView
			for _, hd := range memberPool {
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
			memberSerials = append(memberSerials, pool[0].Serial)
		}
		rr := render.ResolvedRaid{Name: r.Name, Level: r.Level, Mode: mode,
			Members: members, MemberSerials: memberSerials}
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
	// Self-heal a retry: a previous prepare_media pass (or a stranded build)
	// may have left an ISO with this token behind — remove it so the rebuild
	// starts clean instead of accumulating copies in the media export.
	e.cleanupBootMedia(ctx, task, "prepare_media retry rebuild")
	if len(ictx.Resolved.Disks) == 0 && len(ictx.Resolved.Raid) == 0 {
		return classifiedErr("JOB_STAGE_ORDER", false, "layout not resolved; verify_layout must run first")
	}

	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}

	// Access: the root password policy is explicit (mode + optional value —
	// a literal password must never collide with a control value). Random
	// passwords are delivered once via the task event
	// (docs/04-install-spec.md §6).
	// Absent or empty → per-task random, delivered once via the task event
	// (a usable machine needs a usable credential). Any non-empty value is
	// the literal password.
	rootPassword := spec.Access.RootPassword
	if rootPassword == "" {
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
	mediaURI := mediaURIFor(e.MediaBaseURI, filepath.Base(mediaFile))
	distroISO, err := builder.EnsureISO(ctx, spec.Image.Source, e.MediaDir)
	if err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"distro ISO fetch failed: %s", err.Error())
	}
	// Seed from THIS stage's render output: ictx.Answers is the value patched
	// by the previous prepare_media pass (empty on the first pass), so
	// building from it produced a seed-less ISO — ubuntu22 autoinstall then
	// fell back to interactive mode and stalled at the language prompt on
	// real hardware. The context copy below stays for audit and the
	// boot-stage order guard.
	seed := map[string]string{}
	for _, a := range answers {
		seed[a.Name] = a.Content
	}
	// Build in the scratch space when configured (the media repo may be a
	// size-limited share — the extract+assemble needs ~2x the image size
	// transiently), then move the finished image into the repo. Precheck the
	// space and fail fast with real numbers: a mid-write ENOSPC surfaces as
	// an opaque xorriso SORRY/excess-space error instead (real-hardware: an
	// 18G disk filled by accumulated media produced opaque build failures).
	isoStat, err := os.Stat(distroISO)
	if err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true, "distro ISO missing: %s", err.Error())
	}
	needMB := isoStat.Size()/1048576*2 + 512 // extract + assemble + headroom
	outputPath := filepath.Join(e.MediaDir, filepath.Base(mediaFile))
	buildWork := ""
	buildDir := e.MediaWorkDir
	if buildDir == "" {
		buildDir = filepath.Dir(outputPath)
	}
	if e.MediaWorkDir != "" {
		buildWork = filepath.Join(e.MediaWorkDir, filepath.Base(mediaFile)+".build")
		outputPath = filepath.Join(e.MediaWorkDir, filepath.Base(mediaFile))
	}
	if avail, ok := freeMB(buildDir); ok && avail < needMB {
		return classifiedErr("MEDIA_NO_SPACE", true,
			"media build needs ~%d MB free in %s, %d MB available: clear old ISOs or extend the volume",
			needMB, buildDir, avail)
	}
	if avail, ok := freeMB(e.MediaDir); ok && avail < int64(isoStat.Size()/1048576) {
		return classifiedErr("MEDIA_NO_SPACE", true,
			"media repo %s needs ~%d MB free for the boot ISO, %d MB available",
			e.MediaDir, isoStat.Size()/1048576, avail/1048576)
	}
	if berr := buildBootISO(ctx, distroISO, outputPath, boot.KernelArgs, seed, buildWork); berr != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"boot media build failed: %s", berr.Error())
	}
	if buildWork != "" {
		if merr := moveFile(outputPath, filepath.Join(e.MediaDir, filepath.Base(mediaFile))); merr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"boot media move into repo failed: %s", merr.Error())
		}
	}
	// Relay deployment: the BMC mounts the media from the remote export —
	// the file must be complete there BEFORE the boot stage mounts it. The
	// push is synchronous and atomic (temp name + rename), which is what
	// makes the boot-stage race structurally impossible; the settle delay
	// above remains for deployments that still push out-of-band.
	if e.MediaUploader != nil {
		if _, perr := e.MediaUploader.Push(ctx, filepath.Join(e.MediaDir, filepath.Base(mediaFile))); perr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"boot media relay push failed: %s", perr.Error())
		}
		obs.FromContext(ctx).InfoContext(ctx, "boot media relayed", "file", filepath.Base(mediaFile))
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

// bootMediaReleaseGrace is the delay before the completion-triggered media
// release (eject + file reclaim): d-i keeps reading the CD minutes after its
// late_command — the completion report races its finish stage.
const bootMediaReleaseGrace = 3 * time.Minute

// reclaimBootMediaFiles removes the repo, relay and build-scratch copies of
// the task's boot ISO (the file-level half of the deferred release).
func (e *Executor) reclaimBootMediaFiles(ctx context.Context, ictx *installTaskContext) {
	if ictx.Token == "" {
		return
	}
	mediaFile := fmt.Sprintf("boot-%s.iso", ictx.Token)
	if e.MediaDir != "" {
		if err := os.Remove(filepath.Join(e.MediaDir, mediaFile)); err == nil {
			obs.FromContext(ctx).InfoContext(ctx, "boot media removed", "file", mediaFile)
		}
	}
	if e.MediaUploader != nil {
		if err := e.MediaUploader.Remove(ctx, mediaFile); err == nil {
			obs.FromContext(ctx).InfoContext(ctx, "boot media relay copy removed", "file", mediaFile)
		}
	}
	if e.MediaWorkDir != "" {
		_ = os.RemoveAll(filepath.Join(e.MediaWorkDir, mediaFile+".build"))
	}
}

// cleanupBootMedia removes this task's boot ISO (repo copy, relay copy,
// build scratch) — verify_ready removes it on SUCCESS; without this sweep
// every terminally failed or canceled run leaks ~2x the image size into the
// media export (observed: three failed runs filled the disk and poisoned
// every later build).
func (e *Executor) cleanupBootMedia(ctx context.Context, task *store.Task, reason string) {
	var ictx installTaskContext
	if len(task.Context) == 0 || json.Unmarshal(task.Context, &ictx) != nil || ictx.Token == "" {
		return
	}
	mediaFile := fmt.Sprintf("boot-%s.iso", ictx.Token)
	if e.MediaDir != "" {
		if err := os.Remove(filepath.Join(e.MediaDir, mediaFile)); err == nil {
			obs.FromContext(ctx).InfoContext(ctx, "boot media removed",
				"file", mediaFile, "reason", reason)
		}
	}
	if e.MediaUploader != nil {
		if err := e.MediaUploader.Remove(ctx, mediaFile); err == nil {
			obs.FromContext(ctx).InfoContext(ctx, "boot media relay copy removed",
				"file", mediaFile, "reason", reason)
		}
	}
	if e.MediaWorkDir != "" {
		_ = os.RemoveAll(filepath.Join(e.MediaWorkDir, mediaFile+".build"))
	}
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
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	// Mount the builder's boot ISO (media B) — the BMC fetches it via NFS.
	// Eject any existing media first (the slot may be occupied from a
	// previous task or mount_media action).
	bootMedia := bmc.MediaImage{URL: ictx.MediaURI, Kind: bmc.MediaBoot}
	if bootMedia.URL == "" {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", false,
			"boot ISO URI is empty; prepare_media must run first")
	}
	_, _ = e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.EjectMedia(ctx, addr, cred, bootMedia)
	})
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.MountMedia(ctx, addr, cred, bootMedia)
	}); err != nil {
		return err
	}
	// Media settle delay: BMC virtual-media mounts can succeed while the
	// backing file is still arriving (NFS relay deployments — the mount only
	// validates the image header; the firmware reads the payload much later,
	// and a partial read means a dead CD boot). Wait out the transfer.
	if settle := e.BootSettleDelay; settle > 0 {
		obs.FromContext(ctx).InfoContext(ctx, "waiting for boot media to settle",
			"delay", settle.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settle):
		}
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
	// Media B stays mounted through the machine's reboot: with a one-shot CD
	// override the firmware only reads the media tens of seconds into POST,
	// so ejecting at stage entry breaks CD boot on real hardware (Huawei
	// iBMC observed — the installer never came up). The media is ejected
	// once the installer reports completion instead: its job is done by
	// then, and the post-install reboot must not re-enter it
	// (docs/06-install-pipeline.md §3.4).

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
			// Completion reported: the media must NOT be released right away.
			// d-i keeps reading the CD for minutes AFTER late_command (its
			// finish stage re-mounts the cdrom) — ejecting or deleting the
			// ISO now wedges the installer in a "media change" loop on a
			// disconnected virtual drive (real-hardware). Release in the
			// background after a grace period; the pipeline must not block
			// on it. The delayed eject still precedes the installer's own
			// reboot-with-one-shot-CD expiry in every observed run.
			go func(ictx2 installTaskContext) {
				dctx := context.WithoutCancel(ctx)
				time.Sleep(bootMediaReleaseGrace)
				e.ejectBootMediaBestEffort(dctx, task, &ictx2)
				e.reclaimBootMediaFiles(dctx, &ictx2)
				// Installers that stall on a completion dialog (d-i's
				// "Installation complete — remove the media") need the
				// pipeline to reboot them; self-rebooting installers
				// (anaconda, subiquity) must not be touched.
				if !ictx2.Boot.InstallerAutoReboot {
					e.rebootBestEffort(dctx, task)
				}
			}(ictx2)
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

// rebootBestEffort force-restarts the machine, logging (not failing) on
// error — used for installers that stall on a completion dialog instead of
// rebooting themselves (d-i's "Installation complete" screen).
func (e *Executor) rebootBestEffort(ctx context.Context, task *store.Task) {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetPower(ctx, addr, cred, bmc.Cycle)
	}); err != nil {
		obs.FromContext(ctx).WarnContext(ctx, "installer reboot failed (continuing)", "err", err.Error())
	} else {
		obs.FromContext(ctx).InfoContext(ctx, "installer reboot issued")
	}
}

// ejectBootMediaBestEffort ejects the task boot media, logging (not failing)
// on error — the pipeline must not depend on eject latency.
func (e *Executor) ejectBootMediaBestEffort(ctx context.Context, task *store.Task, ictx *installTaskContext) {
	if ictx.Token == "" || ictx.MediaURI == "" {
		return
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return
	}
	bootMedia := bmc.MediaImage{URL: ictx.MediaURI, Kind: bmc.MediaBoot}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.EjectMedia(ctx, addr, cred, bootMedia)
	}); err != nil {
		obs.FromContext(ctx).WarnContext(ctx, "eject boot media failed (continuing)", "err", err.Error())
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

	// The boot media's repo/relay copies are reclaimed by the deferred
	// background release started when the completion report arrived (see
	// installOSStage) — deleting them here, while the installer may still be
	// finishing, truncates its last reads (real-hardware).

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

// moveFile relocates a file, falling back to copy+delete across devices.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// MediaUploader moves assembled boot media into the BMC-reachable share.
// MediaRelay (SSH) is the first implementation; FTP/HTTP relays would slot
// in behind the same two calls.
type MediaUploader interface {
	Push(ctx context.Context, localPath string) (remoteName string, err error)
	Remove(ctx context.Context, name string) error
}

// physicalDrives lists the controller's physical drives via the optional
// enumerator capability (docs/07-bmc.md §5).
func (e *Executor) physicalDrives(ctx context.Context, task *store.Task) ([]bmc.DiskView, error) {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return nil, fmt.Errorf("machine or credential unavailable")
	}
	out, err := e.BMC.Do(ctx, addr, cred, proto, "physical_drives", func(ctx context.Context, d bmc.Driver) (any, error) {
		en, okc := d.(bmc.PhysicalDriveEnumerator)
		if !okc {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "physical_drives",
				Detail: fmt.Sprintf("driver %s does not enumerate physical drives", d.Name())}
		}
		return en.PhysicalDrives(ctx, addr, cred)
	})
	if err != nil {
		return nil, err
	}
	drives, _ := out.([]bmc.DiskView)
	return drives, nil
}

// ── configure_raid (docs/09-roadmap.md M6) ──────────────────────────────────

// configureRaid realizes hardware RAID volumes via the controller
// (Redfish Volume creation, or the fake equivalent). Software RAID needs no
// BMC action — anaconda builds md arrays at install time. Idempotent by
// volume name: existing volumes with the declared name are left alone
// (docs/06-install-pipeline.md §6 retry semantics extended to this stage).
func (e *Executor) configureRaid(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	// Reload: verify_layout advanced the context (resolved raid) after this
	// task snapshot was claimed (docs/06-install-pipeline.md §6 — no stale
	// snapshots across stage boundaries).
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
	_ = json.Unmarshal(task.Context, &ictx)
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
	var boundSize int64
	for _, r := range spec.Storage.Raid {
		if r.Mode != "hardware" {
			continue
		}
		// Member serials come from the resolved raid in the task context —
		// controllers identify drives, and rename volumes, themselves.
		var memberSerials []string
		for _, rr := range ictx.Resolved.Raid {
			if rr.Name == r.Name {
				memberSerials = rr.MemberSerials
			}
		}
		obs.FromContext(ctx).DebugContext(ctx, "raid member resolution",
			"volume", r.Name, "serials", strings.Join(memberSerials, ","),
			"resolved_raid_entries", len(ictx.Resolved.Raid))
		created, cerr := e.BMC.Do(ctx, addr, cred, proto, "create_volume", func(ctx context.Context, d bmc.Driver) (any, error) {
			creator, okc := d.(bmc.VolumeCreator)
			if !okc {
				return nil, classifiedErr("BMC_UNSUPPORTED", false,
					"driver %s does not support volume creation", d.Name())
			}
			return creator.CreateVolume(ctx, addr, cred, bmc.VolumeSpec{
				Name: r.Name, RAIDType: "RAID" + r.Level, MemberSerials: memberSerials,
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
		// The controller may rename the volume (Huawei assigns LogicalDriveN)
		// — bind via the returned name, falling back to the declared one.
		bindings[r.Name], _ = created.(string)
		if bindings[r.Name] == "" {
			bindings[r.Name] = "" // bound below by re-inventory
		}
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
		if bindings[r.Name] != "" {
			continue // the creator already reported the controller's name
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

	// Persist bound names AND the bound volume's capacity — the renderer
	// re-identifies non-kernel bound names by size in %pre.
	for i := range ictx.Resolved.Raid {
		if ictx.Resolved.Raid[i].Mode != "hardware" {
			continue
		}
		if dev := bindings[ictx.Resolved.Raid[i].Name]; dev != "" {
			ictx.Resolved.Raid[i].BoundDevice = dev
			for _, d := range hw.Disks {
				if d.Name == dev {
					ictx.Resolved.Raid[i].SizeBytes = d.SizeBytes
					boundSize = d.SizeBytes
				}
			}
		}
	}
	// Refresh the in-band snapshot when possible: it carries the bound
	// volume's SCSI serial and kernel name as the OS actually sees them —
	// what curtin (ubuntu) and kickstart storage resolution match on.
	_ = e.collectLayout(ctx, task, false)
	if snap, serr := e.layoutSnapshot(ctx, task.MachineID); serr == nil {
		for i := range ictx.Resolved.Raid {
			if ictx.Resolved.Raid[i].Mode != "hardware" || ictx.Resolved.Raid[i].BoundDevice == "" {
				continue
			}
			for _, sd := range snap {
				if withinTolerance(snapshotDiskSize(sd), boundSize) {
					ictx.Resolved.Raid[i].VolumeSerial = sd.Match["serial"]
				}
			}
		}
	}
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
		"raid_bindings": bindings,
		"resolved":      ictx.Resolved,
	}); err != nil {
		return err
	}
	obs.FromContext(ctx).InfoContext(ctx, "hardware raid configured",
		"volumes", len(bindings))
	return nil
}

// withinTolerance reports a within b by ≤1% or 64MiB (controller rounding vs
// kernel reporting; real disks never sit this close by coincidence).
func withinTolerance(a, b int64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	tol := b / 100
	if tol < 64*1024*1024 {
		tol = 64 * 1024 * 1024
	}
	return diff <= tol
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
