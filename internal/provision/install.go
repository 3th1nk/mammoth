package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	// Empty under the pxe boot strategy (the payload is the boot tree).
	MediaURI string `json:"media_uri,omitempty"`
	// BootStrategy records which carrier produced the payload (docs/06 §3).
	// Absent on tasks armed before strategies existed — they were
	// virtual_media by definition, and release treats empty that way.
	BootStrategy string         `json:"boot_strategy,omitempty"`
	Netboot      *netbootRecord `json:"netboot,omitempty"`
	Resolved     struct {
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
		Checksum string `json:"checksum"`
		Distro   string `json:"distro"`
	} `json:"image"`
	Boot struct {
		Strategy string `json:"strategy"`
	} `json:"boot"`
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
	// Self-heal a retry: a previous prepare_media pass may have left a boot
	// ISO or a netboot registration with this token behind — release the old
	// payload so the rebuild starts clean instead of accumulating copies in
	// the media export or stale rows in the registry.
	e.releaseBootPayload(ctx, task, &ictx, "retry")
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

	// PXE install-source inputs (docs/06-install-pipeline.md §3.3): the
	// strategy is known before rendering (spec declaration or deployment
	// default — the same resolution bootStrategyFor performs), so the driver
	// can render its netboot-shaped args/seed up front.
	if name, _ := effectiveStrategyName(e, spec); name == strategyPXE {
		// The carrier decides what the installer consumes over the network.
		// The wimboot carrier (Windows) has NO network install source — the
		// augmented boot.wim carries the answer file and install.wim in the
		// WinPE ramdisk — so Netboot being non-nil is the whole message and
		// the pool/NFS URLs stay empty.
		wimbootCarrier := false
		if driver0, derr := e.Render.For(spec.Image.Distro); derr == nil {
			carrier, _ := render.NetbootInstallOf(driver0)
			wimbootCarrier = carrier == render.NetbootCarrierWimboot
		}
		if wimbootCarrier {
			// The install source is the deployment SMB export — startnet maps
			// it inside WinPE (setup consumes UNC directly, nothing lands in
			// the boot.wim). The prepared tree (the SetupComplete-injected
			// unpack) sits at a sha-addressed path below the share root; the
			// image is located and hashed here so the path is FINAL at render
			// time (EnsureISO is an idempotent cache; prepare reuses both).
			distroISO, ferr := builder.EnsureISO(ctx, spec.Image.Source, e.MediaDir)
			if ferr != nil {
				return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
					"distro ISO fetch failed: %s", ferr.Error())
			}
			sha, herr := builder.FileSHA256(distroISO)
			if herr != nil {
				return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
					"distro ISO hash failed: %s", herr.Error())
			}
			in.Netboot = &render.NetbootInputs{
				InstallSMBUNC:      e.WindowsInstallSMBShare,
				InstallSMBUser:     e.WindowsInstallSMBShareUser,
				InstallSMBPassword: e.WindowsInstallSMBSharePassword,
				// Windows separators, not filepath.Join — this string lands
				// verbatim in the startnet batch script.
				InstallSMBImagePath: PoolStoreDirName + "\\" + sha + "\\win\\tree",
			}
		} else {
			// The shared pool tree is content-addressed by the image's sha256,
			// and the URLs the installer consumes must be FINAL at render time
			// (the d-i mirror rides the preseed body, casper's nfsroot the
			// kernel arguments) — so the image is located and hashed here.
			// EnsureISO is an idempotent cache; prepare reuses both.
			distroISO, ferr := builder.EnsureISO(ctx, spec.Image.Source, e.MediaDir)
			if ferr != nil {
				return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
					"distro ISO fetch failed: %s", ferr.Error())
			}
			sha, herr := builder.FileSHA256(distroISO)
			if herr != nil {
				return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
					"distro ISO hash failed: %s", herr.Error())
			}
			in.Netboot = &render.NetbootInputs{
				// PoolURL points at the shared unpacked-ISO tree (/iso) — d-i's
				// mirror directory and casper's http fallback both root there.
				PoolURL:    fmt.Sprintf("%s/netboot/store/%s/iso", strings.TrimSuffix(e.ExternalURL, "/"), sha),
				NFSRootURL: nfsRootFor(e.MediaBaseURI, filepath.Join(PoolStoreDirName, sha, "iso")),
			}
		}
		// Reserve the machine's pool address now: DHCP-carrier installs (the
		// casper carrier) hand it to the installer as a static ip= argument —
		// the boot-time DHCP is racy on real hardware (udev renames the NIC
		// mid-ipconfig) while the assignment is sticky across reboots. The
		// first inventoried NIC is the provisioning NIC (the same one the
		// spec's match.mac and the netboot entries arm).
		if e.DHCPReserveFor != nil {
			obs.FromContext(ctx).InfoContext(ctx, "reservation probe", "nics", len(hw.NICs), "wired", e.DHCPReserveFor != nil)
			for _, n := range hw.NICs {
				if n.MAC == "" {
					continue
				}
				ip, router, mask := e.DHCPReserveFor(n.MAC)
				obs.FromContext(ctx).InfoContext(ctx, "reservation result", "mac", n.MAC, "ip", ip)
				if ip == nil {
					break
				}
				in.Netboot.StaticIP = ip.String()
				if router != nil {
					in.Netboot.StaticRouter = router.String()
				}
				if mask != nil {
					in.Netboot.StaticMask = netmaskString(mask)
				}
				break
			}
		}
		// The HTTP pool is a signed offline mirror; the installer's apt needs
		// the pool key in its trustdb (see builder.StageNetbootPool). A key
		// failure here is non-fatal at render time — the pool staging below
		// reports it as a media-build failure with the same cause.
		if ent, kerr := builder.PoolSigningEntity(e.MediaDir); kerr == nil {
			if pub, perr := builder.PoolPublicKey(ent); perr == nil {
				in.Netboot.PoolPublicKey = pub
			}
		}
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

	// Hand the render output to the boot carrier: virtual_media assembles
	// boot-<token>.iso for the BMC, pxe extracts the HTTP boot tree and
	// arms the registry (docs/06-install-pipeline.md §3).
	strategy, err := e.bootStrategyFor(spec)
	if err != nil {
		return err
	}
	session := &bootSession{Task: task, Job: job, Spec: spec, Ictx: &ictx, Answers: answers, Boot: boot}
	// The agent install runtime rides the boot media as an apkovl overlay
	// (docs/12-agent-initramfs.md) — plan-independent, built once here, and
	// kept OUT of the rendered answers: the tar.gz is binary and the answers
	// round-trip through the JSON task context.
	if render.IsAgentInstaller(driver) {
		name, overlay, aerr := builder.AgentOverlay()
		if aerr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"agent overlay build failed: %s", aerr.Error())
		}
		session.Seed = map[string]string{name: string(overlay)}
	}
	if err := strategy.prepare(ctx, session); err != nil {
		return err
	}

	raw, _ := json.Marshal(ictx.Install)
	patch := map[string]any{
		"answers":   answers,
		"boot":      boot,
		"media_uri": ictx.MediaURI,
	}
	if ictx.BootStrategy != "" {
		patch["boot_strategy"] = ictx.BootStrategy
	}
	if ictx.Netboot != nil {
		patch["netboot"] = ictx.Netboot
	}
	if raw != nil && string(raw) != "null" {
		patch["install"] = json.RawMessage(raw)
	}
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, patch); err != nil {
		return err
	}
	obs.FromContext(ctx).InfoContext(ctx, "answers rendered, boot payload prepared",
		"files", len(answers), "distro", spec.Image.Distro,
		"strategy", strategy.name(), "media_uri", ictx.MediaURI)
	return nil
}

// bootMediaReleaseGrace is the delay before the completion-triggered media
// release (eject + file reclaim): d-i keeps reading the CD minutes after its
// late_command — the completion report races its finish stage.
const bootMediaReleaseGrace = 3 * time.Minute

// reclaimBootMediaFiles removes the repo, relay and build-scratch copies of
// the task's boot ISO (the file-level half of the virtual-media release).
func (e *Executor) reclaimBootMediaFiles(ctx context.Context, ictx *installTaskContext) {
	if ictx.Token == "" {
		return
	}
	e.removeBootMediaCopies(ctx, fmt.Sprintf("boot-%s.iso", ictx.Token), "")
	if e.MediaWorkDir != "" {
		_ = os.RemoveAll(filepath.Join(e.MediaWorkDir, fmt.Sprintf("boot-%s.iso", ictx.Token)+".build"))
	}
}

// removeBootMediaCopies deletes the repo and relay copies of one boot media
// name, sweeping both the boot/ subdirectory shape and the legacy repo-root
// name — binaries older than the subdirectory isolation left their files at
// the root, and a version upgrade must not strand them (best-effort, logged).
func (e *Executor) removeBootMediaCopies(ctx context.Context, name, reason string) {
	rel := filepath.Join("boot", name)
	if e.MediaDir != "" {
		for _, p := range []string{rel, name} {
			if err := os.Remove(filepath.Join(e.MediaDir, p)); err == nil {
				obs.FromContext(ctx).InfoContext(ctx, "boot media removed", "file", p, "reason", reason)
			}
		}
	}
	if e.MediaUploader != nil {
		for _, n := range []string{rel, name} {
			if err := e.MediaUploader.Remove(ctx, n); err == nil {
				obs.FromContext(ctx).InfoContext(ctx, "boot media relay copy removed", "file", n, "reason", reason)
			}
		}
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
	e.removeBootMediaCopies(ctx, fmt.Sprintf("boot-%s.iso", ictx.Token), reason)
	if e.MediaWorkDir != "" {
		_ = os.RemoveAll(filepath.Join(e.MediaWorkDir, fmt.Sprintf("boot-%s.iso", ictx.Token)+".build"))
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

// bootStage points the firmware at the payload prepare_media produced and
// powers the machine — the carrier-specific sequence lives in the strategy.
func (e *Executor) bootStage(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	fresh, ferr := e.Jobs.GetTask(ctx, task.ID)
	if ferr != nil {
		return ferr
	}
	task = fresh
	ictx := parseInstallContext(task)
	if ictx == nil {
		return classifiedErr("JOB_CONTEXT_CORRUPT", false, "task context unreadable")
	}
	if len(ictx.Answers) == 0 {
		return classifiedErr("JOB_STAGE_ORDER", false, "answers not rendered; prepare_media must run first")
	}
	// The strategy was fixed at prepare time (ictx.BootStrategy): arm with
	// the carrier that actually produced the payload.
	strategy := e.strategyForArmed(ictx)
	return strategy.arm(ctx, &bootSession{Task: task, Job: job, Ictx: ictx})
}

// strategyForArmed resolves the carrier that armed an already-prepared task.
func (e *Executor) strategyForArmed(ictx *installTaskContext) bootStrategy {
	if bootStrategyName(ictx.BootStrategy) == strategyPXE {
		return e.pxe()
	}
	return e.virtualMedia()
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
			// Completion reported: the payload must NOT be released right
			// away. d-i keeps reading the CD for minutes AFTER late_command
			// (its finish stage re-mounts the cdrom) — ejecting or deleting
			// the ISO now wedges the installer in a "media change" loop on a
			// disconnected virtual drive (real-hardware). Release in the
			// background after a grace period; the pipeline must not block
			// on it. PXE has no such tail — its release is registry rows +
			// a directory — but it rides the same grace for uniformity.
			go func(ictx2 installTaskContext) {
				dctx := context.WithoutCancel(ctx)
				grace := bootMediaReleaseGrace
				if bootStrategyName(ictx2.BootStrategy) == strategyPXE {
					// PXE has no CD tail: kernel/initrd were fully loaded at
					// boot, and the installer pulls packages over inst.repo,
					// never the boot tree. Release immediately so the
					// post-install reboot finds an empty registry and grub.cfg
					// falls through to `exit` (boot from disk) instead of
					// re-entering the installer.
					grace = 0
				}
				time.Sleep(grace)
				e.releaseBootPayload(dctx, task, &ictx2, reasonCompleted)
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
	// Context mutates between stages — the completion report lands via
	// RecordInstallComplete while install_os waits — and the runner passes
	// the claim-time snapshot through all stages; re-enter from the fresh
	// record like every other context-reading stage, or the report is
	// invisible here (real-hardware: INSTALL_NOT_VERIFIED on first try).
	fresh, ferr := e.Jobs.GetTask(ctx, task.ID)
	if ferr != nil {
		return ferr
	}
	task = fresh
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
	if e.windowsInstall(ctx, task, job) {
		// The in-band wait below is an SSH probe, and a Windows install has
		// no sshd to answer it — not even the CREDENTIAL_AUTH_FAILED degrade
		// fires — so an on-record credential (a Linux-era leftover, most
		// often) would only burn the wait budget into a false
		// INSTALL_NOT_REACHABLE. The family's verification surface is the
		// completion report alone: SetupComplete fires on the real system's
		// first boot (SYSTEM, pre-logon), which already proves the image
		// applied and the network came up.
		if m.SSHCredentialID != nil && m.SSHAddress != "" {
			obs.FromContext(ctx).WarnContext(ctx, "verify_ready degraded: windows install, in-band ssh probe not applicable",
				"machine", task.MachineID, "ssh_address", m.SSHAddress)
			e.Events.Append(ctx, "task", task.ID, "task.verify_ready_degraded", map[string]any{
				"reason": "windows install; completion report is the verification surface",
			})
		}
	} else if m.SSHCredentialID != nil && m.SSHAddress != "" {
		credRow, err := e.Credentials.Get(ctx, *m.SSHCredentialID)
		if err == nil && credRow.Type == "ssh" {
			plain, derr := e.Crypto.Decrypt(credRow.SecretEncrypted)
			if derr == nil {
				var secret struct {
					Username   string `json:"username"`
					Password   string `json:"password"`
					PrivateKey string `json:"private_key"`
				}
				if json.Unmarshal(plain, &secret) == nil {
					cred := inbandssh.Credentials{
						Username: secret.Username, Password: secret.Password,
						PrivateKey: secret.PrivateKey,
					}
					res, degraded, perr := e.waitForNewSystem(ctx, m.SSHAddress, cred)
					if perr != nil {
						// DHCP-carrier installs (ubuntu PXE: no static
						// declaration allowed) move the machine off the
						// recorded address — its live lease is where the
						// completion report came from. Retry there once.
						if addr, ok := e.dhcpLeaseAddr(m.Hardware); ok {
							obs.FromContext(ctx).WarnContext(ctx, "verify_ready static address unreachable; retrying against the DHCP lease",
								"machine", task.MachineID, "static", m.SSHAddress, "lease", addr)
							res, degraded, perr = e.waitForNewSystem(ctx, addr, cred)
						}
					}
					if perr != nil {
						return classifiedErr("INSTALL_NOT_REACHABLE", true,
							"new system did not answer in-band: %s", perr.Error())
					}
					if degraded {
						// The system answered sshd but rejected authentication:
						// root ssh is intentionally left to the operator's post
						// script (docs/security-baseline.md), so this is an
						// expected state, not a failed install. The completion
						// report is the verification surface; surface it clearly.
						obs.FromContext(ctx).WarnContext(ctx, "verify_ready degraded: system reachable but in-band auth not configured",
							"machine", task.MachineID, "ssh_address", m.SSHAddress)
						e.Events.Append(ctx, "task", task.ID, "task.verify_ready_degraded", map[string]any{
							"reason": "in-band auth not configured; completion report is the verification surface",
						})
					} else {
						// Post-install refresh: the freshly installed system is the
						// best source for the layout snapshot (device names + serials
						// exactly as the installer saw them), so the NEXT reinstall's
						// selectors and bindings match reality without manual steps.
						if version, verr := e.Machines.SaveLayout(ctx, task.MachineID, res.Layout.Source,
							marshalJSON(res.Layout), e.LayoutKeep); verr == nil {
							obs.FromContext(ctx).InfoContext(ctx, "post-install layout snapshot captured",
								"version", version, "disks", len(res.Layout.Disks))
						} else {
							obs.FromContext(ctx).WarnContext(ctx, "post-install snapshot save failed", "err", verr.Error())
						}
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

// verifyReadyWait is the poll budget for the post-install in-band wait.
func (e *Executor) verifyReadyWait() time.Duration {
	if e.VerifyReadyWait > 0 {
		return e.VerifyReadyWait
	}
	return 10 * time.Minute
}

// windowsInstall reports whether the task's resolved spec targets the
// windows family. Resolution failure keeps the legacy in-band path: the
// gate steers verify_ready's verification surface, and a task whose spec
// cannot be re-read must fail loudly downstream rather than silently
// change behavior here (defensive, like the credential chain above).
func (e *Executor) windowsInstall(ctx context.Context, task *store.Task, job *store.Job) bool {
	spec, err := e.loadSpec(ctx, task, job)
	if err != nil {
		return false
	}
	driver, err := e.Render.For(spec.Image.Distro)
	if err != nil {
		return false
	}
	return render.FamilyOf(driver) == "windows"
}

// waitForNewSystem polls the in-band probe until the freshly installed
// system answers — or the wait budget runs out. The completion report
// arrives at the installer's %post/late-command, while the machine still
// has to tear down the installer, reboot, POST and bring up sshd (minutes
// on real hardware — far beyond what task-level retries cover). During
// that window the installer environment's own sshd can answer the probe
// (it carries the kickstart rootpw) — the installer marker filters that
// false positive out, and the loop keeps polling until the real system
// shows up. Auth rejections surface immediately: waiting cannot heal them
// (the installer env accepts passwords the provisioned system refuses).
func (e *Executor) waitForNewSystem(ctx context.Context, addr string, cred inbandssh.Credentials) (*inbandssh.Result, bool, error) {
	wait := e.verifyReadyWait()
	deadline := time.Now().Add(wait)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := e.Inband.Collect(probeCtx, addr, cred)
		cancel()
		if err == nil && !res.InstallerEnv {
			return res, false, nil
		}
		var ie *inbandssh.Error
		if err != nil && errors.As(err, &ie) && ie.Code == "CREDENTIAL_AUTH_FAILED" {
			// The handshake reached authentication, so the system is up and
			// sshd is answering — only the login is not configured (the
			// security posture leaves root ssh to the operator's post script).
			// Degrade rather than fail: "reachable but unauthenticated" is an
			// expected state, and the completion report remains the
			// verification surface.
			return nil, true, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				return nil, false, fmt.Errorf("installer environment still finishing after %s wait", wait)
			}
			return nil, false, err
		}
		obs.FromContext(ctx).InfoContext(ctx, "verify_ready waiting for the new system",
			"installer_env", err == nil, "err", errString(err))
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// netmaskString renders a v4 netmask dotted (net.IPMask.String returns hex).
func netmaskString(mask net.IPMask) string {
	if len(mask) != 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}

// dhcpLeaseAddr resolves the machine's live DHCP lease IP from its hardware
// view's NIC MACs — the verify_ready address fallback for DHCP-carrier
// installs (ubuntu PXE). ok=false when the lease hook is absent (runner
// without the netboot facet) or no NIC holds a live lease.
func (e *Executor) dhcpLeaseAddr(hardware json.RawMessage) (string, bool) {
	if e.DHCPLeaseFor == nil || len(hardware) == 0 {
		return "", false
	}
	var hw struct {
		NICs []struct {
			MAC string `json:"mac"`
		} `json:"nics"`
	}
	if err := json.Unmarshal(hardware, &hw); err != nil {
		return "", false
	}
	for _, n := range hw.NICs {
		if n.MAC == "" {
			continue
		}
		if ip := e.DHCPLeaseFor(n.MAC); ip != nil {
			return ip.String(), true
		}
	}
	return "", false
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
	// Push uploads localPath under the remote export as relName (repo-relative,
	// e.g. "boot/boot-<token>.iso" — subdirectories are created remotely; the
	// BMC-facing URI must mirror the same relative name).
	Push(ctx context.Context, localPath string, relName string) (remoteName string, err error)
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
