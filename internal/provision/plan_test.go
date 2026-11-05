package provision

// PlanInstall regression: the 2288H windows2019 submission shape (the first
// real consumer of the endpoint — it had no coverage and its rejections used
// to surface as 500s because the classified triple carried no Code() method,
// see the api writeError mapping). The happy path also round-trips the spec
// through the contract type, exactly as the API handler does.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/kickstart"
	"github.com/3th1nk/mammoth/internal/render/windows"
)

func windowsPlanRegistry(t *testing.T) *render.Registry {
	t.Helper()
	reg := render.NewRegistry()
	if err := reg.Register(windows.New("windows2019")); err != nil {
		t.Fatal(err)
	}
	return reg
}

// hw2288h is the machine-facing inventory of the verification host: one LSI
// RAID volume, no serial (docs/compat/huawei.md — the controller hides it).
func hw2288h() *bmc.HardwareView {
	return &bmc.HardwareView{
		Disks: []bmc.DiskView{{Name: "LogicalDrive0", Protocol: "raid", SizeBytes: 3999999721472}},
		NICs:  []bmc.NICView{{Name: "mainboardLOMPort1", MAC: "02:00:00:00:00:00"}},
	}
}

const win2288hSpec = `{
  "image": {"source": "file:///data/os_iso/windows2019/cn_windows_server_2019_x64_dvd_4de40f33.iso", "distro": "windows2019"},
  "storage": {"disks": [{"select": {"match": {"size": "largest"}}, "wipe": true, "partitions": [
    {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
    {"size": "rest", "fs": "ntfs", "mount": "/"}]}]},
  "network": [{"match": {"mac": "02:00:00:00:00:00"}, "addresses": ["198.51.100.215/24"],
    "routes": [{"to": "default", "via": "198.51.100.1"}],
    "nameservers": {"addresses": ["198.51.100.3"]}}],
  "identity": {"hostname_pattern": "win-2288h"},
  "boot": {"strategy": "virtual_media"}
}`

func TestPlanInstallWindows2288h(t *testing.T) {
	reg := windowsPlanRegistry(t)

	plan, err := PlanInstall(context.Background(), json.RawMessage(win2288hSpec), hw2288h(), nil, reg)
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if plan.BootDrive != "LogicalDrive0" {
		t.Fatalf("boot_drive = %q, want LogicalDrive0", plan.BootDrive)
	}
	if len(plan.ResolvedDisks) != 1 || len(plan.ResolvedDisks[0].PlannedPartitions) != 2 {
		t.Fatalf("unexpected plan disks: %+v", plan.ResolvedDisks)
	}
	parts := plan.ResolvedDisks[0].PlannedPartitions
	if parts[0].Fs != "vfat" || parts[0].SizeMb == nil || *parts[0].SizeMb != 300 || parts[0].Mount != "/boot/efi" {
		t.Fatalf("ESP partition plan = %+v", parts[0])
	}
	if parts[1].Fs != "ntfs" || parts[1].Grow == nil || !*parts[1].Grow || parts[1].Mount != "/" {
		t.Fatalf("OS partition plan = %+v", parts[1])
	}

	// the handler round-trips the body through the contract type first — the
	// plan must survive that round-trip unchanged (fs enum, selector, MAC).
	var body gen.InstallSpec
	if err := json.Unmarshal([]byte(win2288hSpec), &body); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanInstall(context.Background(), raw, hw2288h(), nil, reg); err != nil {
		t.Fatalf("PlanInstall(contract round-trip): %v", err)
	}
}

func TestPlanInstallRejectionIsClassified(t *testing.T) {
	reg := windowsPlanRegistry(t)

	// selectors against an empty inventory must fail with the machine-readable
	// code a real install would produce — AsClassified is the contract the API
	// error mapper relies on to surface these as 422s, not 500s.
	_, err := PlanInstall(context.Background(), json.RawMessage(win2288hSpec), &bmc.HardwareView{}, nil, reg)
	if err == nil {
		t.Fatal("expected selector failure against empty inventory")
	}
	ci, ok := AsClassified(err)
	if !ok {
		t.Fatalf("rejection is not classified: %v (%T)", err, err)
	}
	if ci.Code != "LAYOUT_DISK_NOT_FOUND" {
		t.Fatalf("code = %q, want LAYOUT_DISK_NOT_FOUND", ci.Code)
	}
}

// storage.root_device_hints floors every selector pool BEFORE the size pick
// (docs/04-install-spec.md §5.1 — the disambiguation backstop); alignment is
// enum-validated and advisory. Both flow through the same classified errors
// the API maps to 422s.
func TestPlanInstallRootDeviceHintsAndAlignment(t *testing.T) {
	reg := windowsPlanRegistry(t)
	hw := &bmc.HardwareView{Disks: []bmc.DiskView{
		{Name: "sda", Protocol: "nvme", SizeBytes: 64 << 30},
		{Name: "sdb", Protocol: "nvme", SizeBytes: 480 << 30},
		// Redfish-blind stacks can hide capacity — an unknown size must
		// not fail the floor.
		{Name: "sdc", Protocol: "raid"},
	}}
	spec := func(storage string) json.RawMessage {
		return json.RawMessage(`{
		  "image": {"source": "file:///x/os.iso", "distro": "windows2019"},
		  "storage": {` + storage + `"disks": [{"select": {"match": {"type": "nvme", "size": "largest"}}, "wipe": true, "partitions": [
		    {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
		    {"size": "rest", "fs": "ntfs", "mount": "/"}]}]},
		  "identity": {"hostname_pattern": "n1"},
		  "boot": {"strategy": "virtual_media"}
		}`)
	}
	codeOf := func(err error) string {
		t.Helper()
		ci, ok := AsClassified(err)
		if !ok {
			t.Fatalf("rejection is not classified: %v (%T)", err, err)
		}
		return ci.Code
	}

	plan, err := PlanInstall(context.Background(), spec(`"root_device_hints": {"min_size_gb": 100}, `), hw, nil, reg)
	if err != nil {
		t.Fatalf("floored plan: %v", err)
	}
	if plan.BootDrive != "sdb" {
		t.Fatalf("boot_drive = %q, want sdb (the floor must keep the 64G stick from winning largest)", plan.BootDrive)
	}

	_, err = PlanInstall(context.Background(), spec(`"root_device_hints": {"min_size_gb": 1024}, `), hw, nil, reg)
	if codeOf(err) != "LAYOUT_DISK_NOT_FOUND" {
		t.Fatalf("starved floor code = %v, want LAYOUT_DISK_NOT_FOUND", err)
	}

	_, err = PlanInstall(context.Background(), spec(`"alignment": "1m", `), hw, nil, reg)
	if codeOf(err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("bad alignment code = %v, want SCHEMA_INVALID_STORAGE", err)
	}
	_, err = PlanInstall(context.Background(), spec(`"root_device_hints": {"min_size_gb": 0}, `), hw, nil, reg)
	if codeOf(err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("non-positive floor code = %v, want SCHEMA_INVALID_STORAGE", err)
	}

	if _, err := PlanInstall(context.Background(), spec(`"alignment": "4k", `), hw, nil, reg); err != nil {
		t.Fatalf("advisory alignment must plan clean: %v", err)
	}

	// unknown-capacity disk (Redfish-blind) survives the floor: the raid
	// selector's pool is sdc alone, floor or not.
	raidSpec := json.RawMessage(`{
	  "image": {"source": "file:///x/os.iso", "distro": "windows2019"},
	  "storage": {"root_device_hints": {"min_size_gb": 100}, "disks": [{"select": {"match": {"protocol": "raid"}}, "wipe": true, "partitions": [
	    {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
	    {"size": "rest", "fs": "ntfs", "mount": "/"}]}]},
	  "identity": {"hostname_pattern": "n1"},
	  "boot": {"strategy": "virtual_media"}
	}`)
	plan, err = PlanInstall(context.Background(), raidSpec, hw, nil, reg)
	if err != nil {
		t.Fatalf("unknown-size disk under floor: %v", err)
	}
	if plan.BootDrive != "sdc" {
		t.Fatalf("boot_drive = %q, want sdc", plan.BootDrive)
	}
}

// storage.lvm (docs/04-install-spec.md §5.1): the dry-run resolves PV
// selectors with the same semantics as verify_layout — overlap, ambiguity
// and structural shape surface with the same classified codes; / on a
// logical volume moves the boot drive to the /boot holder.
func TestPlanInstallLvm(t *testing.T) {
	reg := render.NewRegistry()
	if err := reg.Register(kickstart.New("rocky9")); err != nil {
		t.Fatal(err)
	}
	hw := &bmc.HardwareView{Disks: []bmc.DiskView{
		{Name: "sda", Protocol: "sas", SizeBytes: 480 << 30},
		{Name: "sdb", Protocol: "sas", SizeBytes: 960 << 30},
	}}
	spec := func(storage string) json.RawMessage {
		return json.RawMessage(`{
		  "image": {"source": "file:///x/os.iso", "distro": "rocky9"},
		  "storage": {` + storage + `},
		  "identity": {"hostname_pattern": "n1"},
		  "boot": {"strategy": "virtual_media"}
		}`)
	}
	disks := `"disks": [{"select": {"match": {"size": "smallest"}}, "wipe": true, "partitions": [
	  {"size": "1024M", "fs": "xfs", "mount": "/boot"}]}], `
	freshVG := `"lvm": [{"name": "vg0", "pvs": [{"match": {"size": "largest"}}],
	  "logvols": [{"name": "root", "size": "rest", "fs": "xfs", "mount": "/"}]}]`
	codeOf := func(t *testing.T, err error) string {
		t.Helper()
		ci, ok := AsClassified(err)
		if !ok {
			t.Fatalf("rejection is not classified: %v (%T)", err, err)
		}
		return ci.Code
	}

	// happy path: root rides the LV — the boot drive falls to the /boot holder
	plan, err := PlanInstall(context.Background(), spec(disks+freshVG), hw, nil, reg)
	if err != nil {
		t.Fatalf("lvm plan: %v", err)
	}
	if plan.BootDrive != "sda" {
		t.Fatalf("boot_drive = %q, want sda (the /boot holder; / lives on the LV)", plan.BootDrive)
	}

	// Partition-carve PV (docs/04-install-spec.md §5.1.1): a PV selector may
	// land on a disks[]-claimed holder — the PV is the holder's remaining
	// space as a trailing partition (the single-disk layout: ESP//boot + PV).
	// Boot drive falls to the holder (its /boot partition).
	plan, err = PlanInstall(context.Background(), spec(disks+
		`"lvm": [{"name": "vg0", "pvs": [{"match": {"size": "smallest"}}],
		  "logvols": [{"name": "root", "size": "rest", "fs": "xfs", "mount": "/"}]}]`), hw, nil, reg)
	if err != nil {
		t.Fatalf("partition-carve PV plan: %v", err)
	}
	if plan.BootDrive != "sda" {
		t.Fatalf("boot_drive = %q, want sda (the holder; its /boot is outside LVM)", plan.BootDrive)
	}

	// ...but a holder declaring its own rest partition conflicts — the
	// remaining space belongs to the PV, two growers cannot share it.
	_, err = PlanInstall(context.Background(), spec(
		`"disks": [{"select": {"match": {"size": "smallest"}}, "wipe": true, "partitions": [
	  {"size": "1024M", "fs": "xfs", "mount": "/boot"},
	  {"size": "rest", "fs": "xfs", "mount": "/data"}]}], `+
			`"lvm": [{"name": "vg0", "pvs": [{"match": {"size": "smallest"}}],
		  "logvols": [{"name": "root", "size": "rest", "fs": "xfs", "mount": "/"}]}]`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("holder-grow code = %v, want SCHEMA_INVALID_STORAGE", err)
	}

	// Two fresh groups competing for the same disk: the first consumes it
	// whole (no disks[] claim to carve after) — the second conflicts.
	_, err = PlanInstall(context.Background(), spec(disks+
		`"lvm": [{"name": "vg0", "pvs": [{"match": {"size": "largest"}}],
		  "logvols": [{"name": "data", "size": "10G", "fs": "xfs", "mount": "/data"}]},
		 {"name": "vg1", "pvs": [{"match": {"size": "largest"}}],
		  "logvols": [{"name": "data2", "size": "10G", "fs": "xfs", "mount": "/data2"}]}]`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("PV-vs-PV code = %v, want SCHEMA_INVALID_STORAGE", err)
	}

	// ambiguous PV selector (both SAS disks, no size pick)
	_, err = PlanInstall(context.Background(), spec(disks+
		`"lvm": [{"name": "vg0", "pvs": [{"match": {"protocol": "sas"}}],
		  "logvols": [{"name": "root", "size": "rest", "fs": "xfs", "mount": "/"}]}]`), hw, nil, reg)
	if codeOf(t, err) != "SELECT_AMBIGUOUS" {
		t.Fatalf("ambiguous PV code = %v, want SELECT_AMBIGUOUS", err)
	}

	// structural: only one rest per volume group
	_, err = PlanInstall(context.Background(), spec(disks+
		`"lvm": [{"name": "vg0", "pvs": [{"match": {"size": "largest"}}],
		  "logvols": [{"name": "root", "size": "rest", "fs": "xfs", "mount": "/"},
		              {"name": "data", "size": "rest", "fs": "xfs", "mount": "/data"}]}]`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("double-rest code = %v, want SCHEMA_INVALID_STORAGE", err)
	}

	// unknown distro + lvm still hits the structural validation first — the
	// shape errors are distro-independent
	_, err = PlanInstall(context.Background(), spec(disks+
		`"lvm": [{"name": "vg0", "pvs": [], "logvols": [{"name": "root", "size": "10G", "fs": "xfs", "mount": "/"}]}]`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("pv-less group code = %v, want SCHEMA_INVALID_STORAGE", err)
	}

	// keep:vg binds against the snapshot the handler passes (the same facts
	// verify_layout reads): VG present → resolved; missing VG / snapshot
	// without LVM facts → LAYOUT_VG_NOT_FOUND; fresh name colliding with the
	// machine's existing group → LAYOUT_VG_EXISTS. This is the batch guard:
	// each target machine's plan reads its own reality before submission.
	snap := []inbandssh.VgFacts{{
		Name: "vgdata", PVCount: 1, SizeBytes: 900 << 30,
		PVs: []inbandssh.PvFacts{{Name: "sdb2", SizeBytes: 900 << 30}},
		LVs: []inbandssh.LvFacts{{Name: "lvdata", SizeBytes: 890 << 30, FSType: "xfs"}},
	}}
	keepSpec := func(vg string) json.RawMessage {
		return json.RawMessage(`{
		  "image": {"source": "file:///x/os.iso", "distro": "rocky9"},
		  "storage": {"disks": [{"select": {"match": {"size": "smallest"}}, "wipe": true, "partitions": [
		    {"size": "1024M", "fs": "xfs", "mount": "/boot"},
		    {"size": "rest", "fs": "xfs", "mount": "/"}]}],
		  "lvm": [` + vg + `]},
		  "identity": {"hostname_pattern": "n1"},
		  "boot": {"strategy": "virtual_media"}
		}`)
	}
	keepVG := `{"name": "vgdata", "keep": "vg", "preserve": [{"name": "lvdata", "mount": "/data"}]}`

	plan, err = PlanInstall(context.Background(), keepSpec(keepVG), hw, snap, reg)
	if err != nil {
		t.Fatalf("keep:vg plan with snapshot: %v", err)
	}
	if plan.BootDrive != "sda" {
		t.Fatalf("boot_drive = %q, want sda (/ stays on the plain boot partition)", plan.BootDrive)
	}

	if _, err = PlanInstall(context.Background(), keepSpec(keepVG), hw, nil, reg); codeOf(t, err) != "LAYOUT_VG_NOT_FOUND" {
		t.Fatalf("keep:vg without snapshot code = %v, want LAYOUT_VG_NOT_FOUND", err)
	}
	if _, err = PlanInstall(context.Background(), keepSpec(`{"name": "vgother", "keep": "vg"}`), hw, snap, reg); codeOf(t, err) != "LAYOUT_VG_NOT_FOUND" {
		t.Fatalf("missing VG code = %v, want LAYOUT_VG_NOT_FOUND", err)
	}
	_, err = PlanInstall(context.Background(), spec(disks+
		`"lvm": [{"name": "vgdata", "pvs": [{"match": {"size": "largest"}}],
		  "logvols": [{"name": "root", "size": "rest", "fs": "xfs", "mount": "/"}]}]`), hw, snap, reg)
	if codeOf(t, err) != "LAYOUT_VG_EXISTS" {
		t.Fatalf("fresh-name collision code = %v, want LAYOUT_VG_EXISTS", err)
	}
}
