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

	var warnings []string
	if hw == nil || len(hw.Disks) == 0 {
		warnings = append(warnings, "machine has no hardware inventory; selectors cannot be validated here — run discover first")
	}

	resolved, err := planResolveDisks(spec.Storage.Disks, spec.minDeviceSizeGB(), hw)
	if err != nil {
		return nil, err
	}

	// Boot drive: the disk holding the root partition (same rule the real
	// pipeline uses at prepare_media).
	bootDrive := ""
	for _, rd := range resolved {
		for _, p := range rd.Partitions {
			if p.Mount == "/" {
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
	if _, _, rerr := driver.RenderAnswers(inputs, render.MachineView{ID: "plan"}); rerr != nil {
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

// freeMB reports the available space (MB) on the filesystem holding path.
// ok=false when the platform stat is unavailable (best-effort precheck).
func freeMB(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize) / 1048576, true
}
