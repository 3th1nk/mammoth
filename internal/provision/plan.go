package provision

// Install-plan dry-run (docs/03-api.md §4): resolve an install spec against
// one machine's inventory and return the plan a real install would execute —
// without queuing anything. The validation surface is identical to a real
// submission (SCHEMA_* from parsing, LAYOUT_* / SELECT_AMBIGUOUS from
// resolution, RENDER_FAILED from the driver dialect).
//
// V1 note: the selector resolution here intentionally mirrors verify_layout's
// (install.go resolveDisks) instead of sharing code with it — verify_layout's
// in-band snapshot fallback and keep:partitions snapshot binding need the
// executor context, which the API process does not have. keep: partitions
// therefore resolves the same way a snapshot-less install fails
// (LAYOUT_SNAPSHOT_REQUIRED). The two paths converge when the plan endpoint
// gains snapshot access.

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
)

// PlanInstall resolves specJSON (an InstallSpec, same shape as an install
// job's spec) against hw and validates the dialect renders it. hw=nil means
// the machine has no inventory — selectors fail exactly like a real install
// without discovery.
func PlanInstall(ctx context.Context, specJSON json.RawMessage, hw *bmc.HardwareView, drivers *render.Registry) (*gen.InstallPlan, error) {
	var spec installSpecView
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, classifiedErr("SCHEMA_INVALID_SPEC", false, "spec unreadable: %s", err.Error())
	}
	if spec.Image.Distro == "" {
		return nil, classifiedErr("SCHEMA_INVALID_SPEC", false, "image.distro must be declared explicitly")
	}
	driver, err := drivers.For(spec.Image.Distro)
	if err != nil {
		return nil, classifiedErr("SCHEMA_UNKNOWN_DISTRO", false, "%s", err.Error())
	}
	if err := validateStorageExtras(&spec); err != nil {
		return nil, err
	}
	if err := validateBootExtras(&spec); err != nil {
		return nil, err
	}

	var warnings []string
	if hw == nil || len(hw.Disks) == 0 {
		warnings = append(warnings, "machine has no hardware inventory; selectors cannot be validated here — run discover first")
	}

	resolved, err := planResolveDisks(spec.Storage.Disks, spec.minDeviceSizeGB(), hw)
	if err != nil {
		return nil, err
	}
	claimed := map[string]string{}
	for _, rd := range resolved {
		claimed[rd.Device] = "disks"
	}

	// LVM PVs resolve against the same inventory with the same selector
	// semantics (the dry-run mirror of verify_layout). keep:vg groups bind
	// to the machine's layout snapshot in the executor, not in the API
	// process — the plan reads as LAYOUT_SNAPSHOT_REQUIRED, same as
	// keep:partitions does.
	resolvedLvm, err := planResolveLvm(spec.Storage.Lvm, spec.Storage.Raid, spec.minDeviceSizeGB(), hw, claimed)
	if err != nil {
		return nil, err
	}

	// Boot drive: the disk holding the root partition (same rule the real
	// pipeline uses at prepare_media). When / lives on a logical volume the
	// non-LVM /boot holder is the bootloader target instead.
	rootOnLV := false
	for _, vg := range resolvedLvm {
		for _, lv := range vg.Logvols {
			if lv.Mount == "/" {
				rootOnLV = true
			}
		}
	}
	rootMount, bootMount := "/", "/"
	if rootOnLV {
		rootMount, bootMount = "/boot", "/boot"
	}
	bootDrive := ""
	for _, rd := range resolved {
		for _, p := range rd.Partitions {
			if p.Mount == rootMount || (rootOnLV && p.Mount == bootMount) {
				bootDrive = rd.Device
			}
		}
	}

	// Render validation: the dialect must be able to express this layout
	// (the same RenderAnswers the real install runs). Any dialect limit
	// surfaces here as RENDER_FAILED instead of stalling an installer on
	// real hardware.
	inputs := render.InstallInputs{
		TaskToken:     "plan",
		MachineID:     "plan",
		Hostname:      "plan",
		ImageSource:   spec.Image.Source,
		RootPassword:  "plan",
		BootDrive:     bootDrive,
		Disks:         resolved,
		AnswerBaseURL: "http://plan/render/plan",
		CompleteURL:   "http://plan/render/plan/complete",
		AgentLogURL:   "http://plan/render/plan/agentlog",
		// The declared kernel arguments ride along so dialect gates (the
		// windows rejection) fire in the dry-run exactly as they will in
		// the real render.
		ExtraKernelArgs:     spec.Boot.KernelArgs,
		InstalledKernelArgs: spec.Boot.InstalledKernelArgs,
	}
	for _, n := range spec.Network {
		inputs.Network = append(inputs.Network, n.toRender())
	}
	for _, r := range spec.PackageSource.Repos {
		inputs.PackageSource = append(inputs.PackageSource, render.RepoSpec{
			Name: r.Name, URL: r.URL, GPGKey: r.GPGKeyURL,
			Suite: r.Suite, Components: r.Components,
		})
	}
	for _, r := range spec.Storage.Raid {
		rr := render.ResolvedRaid{Name: r.Name, Level: r.Level, Mode: r.Mode}
		for i, p := range r.Partitions {
			number := p.Number
			if number == 0 {
				number = i + 1
			}
			mb, grow := 0, false
			if p.Size == "rest" {
				grow = true
			} else if m, err := sizeToMB(p.Size); err == nil {
				mb = m
			}
			rr.Partitions = append(rr.Partitions, render.ResolvedPartition{
				Number: number, Mount: p.Mount, FS: p.FS, SizeMB: mb, Grow: grow, Flags: p.Flags,
			})
		}
		inputs.Raid = append(inputs.Raid, rr)
	}
	inputs.Lvm = resolvedLvm
	// Hardware rides along: dialects resolve controller-assigned disk names
	// (LogicalDriveN) against the inventory by capacity during render
	// validation — an empty MachineView made debian13 RAID-volume machines
	// die on "capacity is unknown" while the real pipeline (which passes
	// hardware) installs fine (2288H 2026-11-03).
	if _, _, rerr := driver.RenderAnswers(inputs, render.MachineView{ID: "plan", Hardware: hw}); rerr != nil {
		return nil, classifiedErr("RENDER_FAILED", false, "%s", rerr.Error())
	}

	disks := make([]gen.InstallPlanDisk, 0, len(resolved))
	for _, rd := range resolved {
		serial := rd.Serial
		keep := rd.KeepDisk
		disks = append(disks, gen.InstallPlanDisk{
			Device:            rd.Device,
			Serial:            &serial,
			SizeBytes:         rd.SizeBytes,
			Keep:              &keep,
			PlannedPartitions: planPartitions(rd.Partitions),
		})
	}
	return &gen.InstallPlan{
		Driver:        spec.Image.Distro,
		ResolvedDisks: disks,
		BootDrive:     bootDrive,
	}, nil
}

// planPartitions maps resolved partitions to the plan view.
func planPartitions(parts []render.ResolvedPartition) []gen.InstallPlanPartition {
	out := make([]gen.InstallPlanPartition, 0, len(parts))
	for i, p := range parts {
		number := p.Number
		if number == 0 {
			number = i + 1 // declaration order (the on-disk contract)
		}
		part := gen.InstallPlanPartition{
			Number: number,
			Fs:     p.FS,
			Mount:  p.Mount,
			Grow:   boolPtr(p.Grow),
		}
		if p.SizeMB > 0 {
			part.SizeMb = intPtr(p.SizeMB)
		}
		if len(p.Flags) > 0 {
			part.Flags = &p.Flags
		}
		out = append(out, part)
	}
	return out
}

// planResolveDisks mirrors verify_layout's storage resolution for the plan
// view (same selector semantics, same error codes). minSizeGB is the
// storage.root_device_hints floor — nil when the spec declares none.
func planResolveDisks(disks []storageDiskView, minSizeGB *int, hw *bmc.HardwareView) ([]render.ResolvedDisk, error) {
	resolved := make([]render.ResolvedDisk, 0, len(disks))
	used := map[string]int{}
	for i, d := range disks {
		match := d.Select.Match
		var pool []bmc.DiskView
		for _, hd := range hw.Disks {
			if !diskMatchesSelector(match, minSizeGB, hd) {
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
		if len(candidates) == 0 {
			return nil, classifiedErr("LAYOUT_DISK_NOT_FOUND", false,
				"storage.disks[%d]: selector matches no disk", i)
		}
		if len(candidates) > 1 {
			return nil, classifiedErr("SELECT_AMBIGUOUS", false,
				"storage.disks[%d]: selector matches %d disks; refine the match", i, len(candidates))
		}
		device := candidates[0].Name
		if prev, dup := used[device]; dup {
			return nil, classifiedErr("SCHEMA_INVALID_STORAGE", false,
				"storage.disks[%d] resolves to %s, already claimed by disks[%d]", i, device, prev)
		}
		used[device] = i
		rd := render.ResolvedDisk{Device: device, Serial: candidates[0].Serial, SizeBytes: candidates[0].SizeBytes}

		switch {
		case d.Keep == "disk":
			rd.KeepDisk = true
		default:
			if !d.Wipe {
				return nil, classifiedErr("SCHEMA_INVALID_STORAGE", false,
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
						return nil, classifiedErr("SCHEMA_INVALID_STORAGE", false,
							"storage.disks[%d].partitions: %s", i, err.Error())
					}
					rp.SizeMB = mb
				}
				rd.Partitions = append(rd.Partitions, rp)
			}
		}
		resolved = append(resolved, rd)
	}
	return resolved, nil
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// planResolveLvm mirrors verify_layout's LVM resolution for the plan view
// (same selector semantics, same error codes). claimed carries the devices
// disks[] already resolved to (device → claimer) so PV overlap surfaces here
// exactly as the real pipeline reports it. keep:vg groups always fail with
// LAYOUT_SNAPSHOT_REQUIRED: snapshot binding needs the executor context.
func planResolveLvm(vgs []volumeGroupView, raids []raidSpecView, minSizeGB *int, hw *bmc.HardwareView, claimed map[string]string) ([]render.ResolvedVolgroup, error) {
	resolved := make([]render.ResolvedVolgroup, 0, len(vgs))
	raidByName := map[string]raidSpecView{}
	for _, r := range raids {
		raidByName[r.Name] = r
	}
	for i, vg := range vgs {
		if vg.Keep == "vg" {
			return nil, classifiedErr("LAYOUT_SNAPSHOT_REQUIRED", false,
				"lvm[%d]: keep:vg binds to the machine's layout snapshot, which the plan endpoint cannot read — submit the install to verify against the machine", i)
		}
		rg := render.ResolvedVolgroup{Name: vg.Name}
		for j, pv := range vg.PVs {
			switch {
			case pv.Match != nil:
				match := *pv.Match
				var pool []bmc.DiskView
				for _, hd := range hw.Disks {
					if !diskMatchesSelector(match, minSizeGB, hd) {
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
				switch {
				case len(candidates) == 0:
					return nil, classifiedErr("LAYOUT_DISK_NOT_FOUND", false,
						"lvm[%d].pvs[%d]: selector matches no disk", i, j)
				case len(candidates) > 1:
					return nil, classifiedErr("SELECT_AMBIGUOUS", false,
						"lvm[%d].pvs[%d]: selector matches %d disks; refine the match", i, j, len(candidates))
				}
				device := candidates[0].Name
				if prev, dup := claimed[device]; dup {
					return nil, classifiedErr("SCHEMA_INVALID_STORAGE", false,
						"lvm[%d].pvs[%d] resolves to %s, already claimed by %s", i, j, device, prev)
				}
				claimed[device] = fmt.Sprintf("lvm[%d]", i)
				rg.PVs = append(rg.PVs, render.ResolvedPV{
					Device: device, Serial: candidates[0].Serial, SizeBytes: candidates[0].SizeBytes,
				})
			case pv.Raid != "":
				rr := raidByName[pv.Raid] // validateLvmShape guarantees presence
				rg.PVs = append(rg.PVs, render.ResolvedPV{
					RaidName: rr.Name, RaidMode: rr.Mode, RaidLevel: rr.Level,
				})
			}
		}
		for _, lv := range vg.Logvols {
			rl := render.ResolvedLogvol{Name: lv.Name, FS: lv.FS, Mount: lv.Mount, Flags: lv.Flags}
			if lv.Size == "rest" {
				rl.Grow = true
			} else {
				mb, err := sizeToMB(lv.Size)
				if err != nil {
					return nil, classifiedErr("SCHEMA_INVALID_STORAGE", false,
						"lvm[%d].logvols: %s", i, err.Error())
				}
				rl.SizeMB = mb
			}
			rg.Logvols = append(rg.Logvols, rl)
		}
		resolved = append(resolved, rg)
	}
	return resolved, nil
}

// freeMB reports the available space (MB) on the filesystem holding path.
// ok=false when the platform stat is unavailable (best-effort precheck).
func freeMB(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize) / 1048576, true
}
