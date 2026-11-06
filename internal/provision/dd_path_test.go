package provision

// The dd pathway's submit gates (docs/09「镜像直写通路」slice 1): the dry-run
// shares the executor's parse surface, so every honest 422 lands here too.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render/dd"
)

func ddPlanRegistry(t *testing.T) *render.Registry {
	t.Helper()
	reg := render.NewRegistry()
	if err := reg.Register(dd.New("dd")); err != nil {
		t.Fatal(err)
	}
	return reg
}

func ddSpec(storage string) json.RawMessage {
	return json.RawMessage(`{
	  "image": {"source": "http://10.0.0.9/images/thor.img.gz", "checksum": "sha256:abcdef0123456789", "distro": "dd", "format": "raw.gz"},
	  "storage": {` + storage + `},
	  "boot": {"strategy": "pxe"}
	}`)
}

func TestPlanInstallDD(t *testing.T) {
	reg := ddPlanRegistry(t)
	hw := &bmc.HardwareView{Disks: []bmc.DiskView{{Name: "nvme0n1", SizeBytes: 4 << 30}}}
	// happy path: one wiped target, no partitions, PXE
	plan, err := PlanInstall(context.Background(), ddSpec(`"disks": [{"select": {"match": {}}, "wipe": true}]`), hw, nil, reg)
	if err != nil {
		t.Fatalf("dd plan: %v", err)
	}
	if len(plan.ResolvedDisks) != 1 || plan.ResolvedDisks[0].Device != "nvme0n1" {
		t.Fatalf("resolved disks = %+v", plan.ResolvedDisks)
	}
	codeOf := func(t *testing.T, err error) string {
		t.Helper()
		ci, ok := AsClassified(err)
		if !ok {
			t.Fatalf("rejection is not classified: %v (%T)", err, err)
		}
		return ci.Code
	}
	// intent blocks are honest 422s until the NoCloud seed lands (slice 2)
	_, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img", "checksum": "sha256:ab", "distro": "dd"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "identity": {"hostname_pattern": "n1"},
	  "boot": {"strategy": "pxe"}}`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_SPEC" {
		t.Fatalf("identity-intent code = %v, want SCHEMA_INVALID_SPEC", err)
	}
	// checksum is mandatory (the write verification compares the stream)
	_, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img", "distro": "dd"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "boot": {"strategy": "pxe"}}`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_SPEC" || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("no-checksum err = %v", err)
	}
	// partitions/lvm are not expressible — the image owns the layout
	_, err = PlanInstall(context.Background(), ddSpec(`"disks": [{"select": {"match": {}}, "wipe": true, "partitions": [
	  {"size": "1G", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]}]}], `+
		`"lvm": []`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_SPEC" {
		t.Fatalf("partitions code = %v, want SCHEMA_INVALID_SPEC", err)
	}
	// vMedia is rejected — the alpine netboot carrier has no ISO assembly
	_, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img", "checksum": "sha256:ab", "distro": "dd"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "boot": {"strategy": "virtual_media"}}`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_SPEC" {
		t.Fatalf("vmedia code = %v, want SCHEMA_INVALID_SPEC", err)
	}
}
