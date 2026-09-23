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
	"github.com/3th1nk/mammoth/internal/render"
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

	plan, err := PlanInstall(context.Background(), json.RawMessage(win2288hSpec), hw2288h(), reg)
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
	if _, err := PlanInstall(context.Background(), raw, hw2288h(), reg); err != nil {
		t.Fatalf("PlanInstall(contract round-trip): %v", err)
	}
}

func TestPlanInstallRejectionIsClassified(t *testing.T) {
	reg := windowsPlanRegistry(t)

	// selectors against an empty inventory must fail with the machine-readable
	// code a real install would produce — AsClassified is the contract the API
	// error mapper relies on to surface these as 422s, not 500s.
	_, err := PlanInstall(context.Background(), json.RawMessage(win2288hSpec), &bmc.HardwareView{}, reg)
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

	plan, err := PlanInstall(context.Background(), spec(`"root_device_hints": {"min_size_gb": 100}, `), hw, reg)
	if err != nil {
		t.Fatalf("floored plan: %v", err)
	}
	if plan.BootDrive != "sdb" {
		t.Fatalf("boot_drive = %q, want sdb (the floor must keep the 64G stick from winning largest)", plan.BootDrive)
	}

	_, err = PlanInstall(context.Background(), spec(`"root_device_hints": {"min_size_gb": 1024}, `), hw, reg)
	if codeOf(err) != "LAYOUT_DISK_NOT_FOUND" {
		t.Fatalf("starved floor code = %v, want LAYOUT_DISK_NOT_FOUND", err)
	}

	_, err = PlanInstall(context.Background(), spec(`"alignment": "1m", `), hw, reg)
	if codeOf(err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("bad alignment code = %v, want SCHEMA_INVALID_STORAGE", err)
	}
	_, err = PlanInstall(context.Background(), spec(`"root_device_hints": {"min_size_gb": 0}, `), hw, reg)
	if codeOf(err) != "SCHEMA_INVALID_STORAGE" {
		t.Fatalf("non-positive floor code = %v, want SCHEMA_INVALID_STORAGE", err)
	}

	if _, err := PlanInstall(context.Background(), spec(`"alignment": "4k", `), hw, reg); err != nil {
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
	plan, err = PlanInstall(context.Background(), raidSpec, hw, reg)
	if err != nil {
		t.Fatalf("unknown-size disk under floor: %v", err)
	}
	if plan.BootDrive != "sdc" {
		t.Fatalf("boot_drive = %q, want sdc", plan.BootDrive)
	}
}
