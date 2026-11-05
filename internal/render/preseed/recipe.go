package preseed

import (
	"fmt"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// target is the single install target partman-auto will work on: one disk —
// or one bound hardware-RAID volume — with its declared partitions.
// partman-auto/disk takes exactly one device, so a multi-target wipe layout
// is a hard limit for this dialect (keep: disk on the rest is the shape).
type target struct {
	device     string // kernel name; partman-auto/disk accepts that form only
	sizeBytes  int64
	partitions []render.ResolvedPartition
	// vgName non-empty switches the recipe to LVM: the PV unit plus the
	// volume group's logical volumes ride the same single-target recipe
	// (partman-auto has no multi-disk shape — docs/compat/distros.md debian).
	vgName  string
	logvols []render.ResolvedLogvol
	// dynamic marks a controller-named hardware-RAID volume (LogicalDrive0,
	// not a kernel name): the preseed leaves partman-auto/disk and
	// grub-installer/bootdev unset and the early_command hook resolves the
	// kernel device by size, seeding both via debconf-set (the official
	// dynamic-preseed shape — the same re-identification kickstart does in %pre).
	dynamic bool
}

// growMaxMB is the expert_recipe "max" for rest-size partitions — partman
// clamps the allocation to the available space (the official recipe idiom).
const growMaxMB = 1000000000

// installTarget resolves the install target and enforces the dialect limits.
// Every rejection names the distro like the ubuntu22 driver does — executors
// surface these verbatim as RENDER_FAILED.
func installTarget(distro string, in render.InstallInputs, m render.MachineView) (target, error) {
	prefix := distro + ": "

	// Hardware RAID volumes are install targets like disks (the bound device
	// came from configure_raid's re-inventory). Software RAID is not
	// reachable: partman md recipes are a different dialect.
	candidates := []target{}
	raidPVNames := map[string]bool{}
	for _, vg := range in.Lvm {
		for _, pv := range vg.PVs {
			if pv.RaidName != "" && pv.RaidMode == "hardware" {
				raidPVNames[pv.RaidName] = true
			}
		}
	}
	for _, r := range in.Raid {
		if r.Mode != "hardware" {
			return target{}, fmt.Errorf("%ssoftware RAID is not supported by the debian-installer preseed driver; use plain disks or a hardware RAID volume (bound device)", prefix)
		}
		if r.BoundDevice == "" {
			continue // not yet bound — configure_raid decides, render only sees bound volumes
		}
		candidates = append(candidates, target{device: r.BoundDevice, sizeBytes: r.SizeBytes, partitions: r.Partitions})
	}
	for _, disk := range in.Disks {
		if disk.KeepDisk {
			continue // kept whole: never a partman-auto target — the partial keep semantics
		}
		if len(disk.Baseline) > 0 || len(disk.Remove) > 0 || hasPreserve(disk.Partitions) {
			return target{}, fmt.Errorf("%skeep: partitions is not supported on this distro (partial); submit without preserve", prefix)
		}
		candidates = append(candidates, target{device: disk.Device, sizeBytes: diskSizeOf(m, disk.Device), partitions: disk.Partitions})
	}
	// A fresh volume group's PV holder joins the candidate pool when the
	// operator declared no partitions on it — the natural error then names
	// the missing recipe partitions instead of "storage config is empty".
	for _, vg := range in.Lvm {
		if vg.KeepVG || len(vg.PVs) != 1 || vg.PVs[0].Device == "" {
			continue
		}
		pv := vg.PVs[0]
		known := false
		for _, c := range candidates {
			if c.device == pv.Device {
				known = true
			}
		}
		if !known {
			candidates = append(candidates, target{device: pv.Device, sizeBytes: pv.SizeBytes})
		}
	}

	if len(candidates) == 0 {
		return target{}, fmt.Errorf("%sstorage config is empty", prefix)
	}
	if len(candidates) > 1 {
		devices := make([]string, 0, len(candidates))
		for _, c := range candidates {
			devices = append(devices, c.device)
		}
		return target{}, fmt.Errorf("%sonly one install target disk is supported (partman-auto); declare keep: disk on all but one of %v", prefix, devices)
	}

	t := candidates[0]
	if !render.IsKernelDeviceName(t.device) {
		// Controller-assigned names (LogicalDriveN) are re-identified by size
		// in the early_command hook — impossible without a known capacity.
		if t.sizeBytes <= 0 {
			return target{}, fmt.Errorf("%sinstall target %s is not a kernel device name and its capacity is unknown (no inventory snapshot); cannot resolve it in the installer", prefix, t.device)
		}
		t.dynamic = true
	}
	if len(t.partitions) == 0 {
		return target{}, fmt.Errorf("%sinstall target %s declares no partitions (partman-auto needs an expert recipe)", prefix, t.device)
	}

	growCount := 0
	for _, p := range t.partitions {
		switch {
		case hasFlag(p.Flags, "esp"), hasFlag(p.Flags, "biosgrub"), p.Mount == "swap":
			// method-driven: fs comes from the flag/mount, not the spec's FS
		default:
			if !supportedFS(p.FS) {
				return target{}, fmt.Errorf("%sfilesystem %s is not available in the netinst installer; use ext4 (or vfat/swap)", prefix, p.FS)
			}
		}
		if p.Grow {
			growCount++
		}
	}
	if growCount > 1 {
		return target{}, fmt.Errorf("%sonly one grow partition per disk is supported (partman assigns free space to one partition)", prefix)
	}

	// LVM (docs/compat/distros.md debian): partman-auto sees one install
	// target, so the dialect's shape is one volume group with exactly one
	// PV on that target. keep:vg cannot work at all — the recipe mechanism
	// has no way to adopt an existing group.
	if len(in.Lvm) > 0 {
		vg, err := lvmForTarget(distro, in, t)
		if err != nil {
			return target{}, err
		}
		t.vgName = vg.Name
		t.logvols = vg.Logvols
	}
	return t, nil
}

// lvmForTarget resolves the debian dialect's LVM shape against the single
// install target: exactly one fresh volume group, exactly one PV, and that
// PV must BE the target (partman's expert_recipe carries the PV unit and the
// LV units on one device; a VG spanning other disks is not expressible).
func lvmForTarget(distro string, in render.InstallInputs, t target) (render.ResolvedVolgroup, error) {
	prefix := distro + ": "
	if len(in.Lvm) > 1 {
		return render.ResolvedVolgroup{}, fmt.Errorf("%sonly one volume group is supported (partman-auto works on a single recipe)", prefix)
	}
	vg := in.Lvm[0]
	if vg.KeepVG {
		return render.ResolvedVolgroup{}, fmt.Errorf(
			"%skeeping the existing volume group %s is not supported by the debian-installer (partman cannot adopt an existing group); reinstall its data separately", prefix, vg.Name)
	}
	if len(vg.PVs) != 1 {
		return render.ResolvedVolgroup{}, fmt.Errorf(
			"%svolume group %s needs exactly one pv (partman-auto sees one install target); got %d", prefix, vg.Name, len(vg.PVs))
	}
	pv := vg.PVs[0]
	holder := pv.Device
	if pv.RaidName != "" {
		if pv.RaidMode != "hardware" {
			return render.ResolvedVolgroup{}, fmt.Errorf(
				"%ssoftware RAID is not supported by the debian-installer preseed driver", prefix)
		}
		holder = pv.RaidBoundDevice
	}
	if holder != t.device {
		return render.ResolvedVolgroup{}, fmt.Errorf(
			"%svolume group %s PV holder (%s) must be the single install target (%s); partman-auto has no multi-disk recipe", prefix, vg.Name, holder, t.device)
	}
	for _, lv := range vg.Logvols {
		if lv.FS != "swap" && !supportedFS(lv.FS) {
			return render.ResolvedVolgroup{}, fmt.Errorf(
				"%sfilesystem %s is not available in the netinst installer; use ext4 (or vfat/swap)", prefix, lv.FS)
		}
		if lv.FS != "swap" && lv.Mount != "swap" && lv.Mount == "" {
			return render.ResolvedVolgroup{}, fmt.Errorf(
				"%slogical volume %s needs a mountpoint (or fs swap)", prefix, lv.Name)
		}
	}
	return vg, nil
}

// expertRecipe renders the partman-auto/expert_recipe body: one unit per
// declared partition, in declaration order (partman assigns partition
// numbers in recipe order — spec order IS on-disk order). Lines carry the
// preseed backslash-continuation; the recipe itself is plain text (no
// quotes, no backslashes), so it is safe inside a multi-line preseed value.
func expertRecipe(t target) (string, error) {
	var b strings.Builder
	b.WriteString("  mammoth :: \\\n")
	for _, p := range t.partitions {
		minMB := p.SizeMB
		if minMB <= 0 {
			minMB = 1
		}
		maxMB := minMB
		if p.Grow {
			maxMB = growMaxMB
		}

		unit := ""
		switch {
		case hasFlag(p.Flags, "biosgrub"):
			// GPT BIOS boot partition: no filesystem, no format.
			unit = fmt.Sprintf("      %d %d %d free \\\n        $primary{ } \\\n        method{ biosgrub } \\\n      . \\\n", minMB, minMB, maxMB)
		case hasFlag(p.Flags, "esp"):
			// EFI system partition: the efi method — partman mounts it at
			// /boot/efi itself (the official recipe carries no mountpoint).
			unit = fmt.Sprintf("      %d %d %d free \\\n        $primary{ } \\\n        method{ efi } format{ } \\\n      . \\\n", minMB, minMB, maxMB)
		case p.Mount == "swap":
			unit = fmt.Sprintf("      %d %d %d linux-swap \\\n        method{ swap } format{ } \\\n      . \\\n", minMB, minMB, maxMB)
		default:
			unit = fmt.Sprintf("      %d %d %d %s \\\n        $primary{ } \\\n        method{ format } format{ } \\\n        use_filesystem{ } filesystem{ %s } \\\n        mountpoint{ %s } \\\n      . \\\n", minMB, minMB, maxMB, p.FS, p.FS, p.Mount)
		}
		b.WriteString(unit)
	}
	// LVM recipe tail: the PV unit turns the target's remaining space into
	// the group's physical volume; the LV units then live inside it
	// ($lvmok{ } resolves to VG free space, the official partman-auto-lvm
	// recipe shape). A grow LV takes the same max clamp as a grow partition.
	if t.vgName != "" {
		b.WriteString(fmt.Sprintf("      1 1 %d free \\\n        $primary{ } \\\n        method{ lvm } vg_name{ %s } \\\n      . \\\n", growMaxMB, t.vgName))
		for _, lv := range t.logvols {
			minMB := lv.SizeMB
			if minMB <= 0 {
				minMB = 1
			}
			maxMB := minMB
			if lv.Grow {
				maxMB = growMaxMB
			}
			unit := ""
			if lv.FS == "swap" || lv.Mount == "swap" {
				unit = fmt.Sprintf("      %d %d %d linux-swap \\\n        $lvmok{ } \\\n        in_vg { %s } lv_name{ %s } \\\n        method{ swap } format{ } \\\n      . \\\n", minMB, minMB, maxMB, t.vgName, lv.Name)
			} else {
				unit = fmt.Sprintf("      %d %d %d %s \\\n        $lvmok{ } \\\n        in_vg { %s } lv_name{ %s } \\\n        method{ format } format{ } \\\n        use_filesystem{ } filesystem{ %s } \\\n        mountpoint{ %s } \\\n      . \\\n", minMB, minMB, maxMB, lv.FS, t.vgName, lv.Name, lv.FS, lv.Mount)
			}
			b.WriteString(unit)
		}
	}
	out := b.String()
	// The last unit terminates the value — drop its continuation so the
	// following preseed line starts a new statement.
	return strings.TrimSuffix(out, " \\\n") + "\n", nil
}

// supportedFS is the netinst partman whitelist. xfs needs partman-xfs in the
// installer image (unverified on netinst) — rejected until confirmed.
func supportedFS(fs string) bool {
	switch fs {
	case "ext4", "ext3", "ext2", "vfat", "fat32":
		return true
	}
	return false
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func hasPreserve(parts []render.ResolvedPartition) bool {
	for _, p := range parts {
		if p.Preserve {
			return true
		}
	}
	return false
}

// diskSizeOf finds a hardware disk's capacity by inventory name.
func diskSizeOf(m render.MachineView, device string) int64 {
	if m.Hardware == nil {
		return 0
	}
	for _, hd := range m.Hardware.Disks {
		if hd.Name == device {
			return hd.SizeBytes
		}
	}
	return 0
}
