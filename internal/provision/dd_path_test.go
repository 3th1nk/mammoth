package provision

// The dd pathway's submit gates (docs/09「镜像直写通路」slice 1): the dry-run
// shares the executor's parse surface, so every honest 422 lands here too.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
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
	// identity/network/access intent rides the NoCloud seed (slice 2)
	if _, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img", "checksum": "sha256:ab", "distro": "dd"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "identity": {"hostname_pattern": "n1"},
	  "access": {"root_password": "s3cret-xy"},
	  "network": [{"match": {"mac": "aa:bb:cc:dd:ee:01"}, "addresses": ["192.168.1.9/24"]}],
	  "boot": {"strategy": "pxe"}}`), hw, nil, reg); err != nil {
		t.Fatalf("dd plan with seed intent: %v", err)
	}
	// raw.xz passes the format gate (pool gating is prepare-time)
	if _, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img.xz", "checksum": "sha256:ab", "distro": "dd", "format": "raw.xz"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "boot": {"strategy": "pxe"}}`), hw, nil, reg); err != nil {
		t.Fatalf("dd plan raw.xz: %v", err)
	}
	// bond intent stays an honest reject (the seed carries plain interfaces)
	_, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img", "checksum": "sha256:ab", "distro": "dd"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "network": [{"bond": {"interfaces": [{"match": {"mac": "aa:bb:cc:dd:ee:01"}, "match": {"mac": "aa:bb:cc:dd:ee:02"}}], "mode": "active-backup"}}],
	  "boot": {"strategy": "pxe"}}`), hw, nil, reg)
	if codeOf(t, err) != "SCHEMA_INVALID_SPEC" {
		t.Fatalf("bond code = %v, want SCHEMA_INVALID_SPEC", err)
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
	// vMedia is a supported carrier since the dual-carrier round (the alpine
	// standard ISO boots the agent; the machine streams the image) — the
	// plan renders it without error.
	if _, err = PlanInstall(context.Background(), json.RawMessage(`{
	  "image": {"source": "http://x/i.img", "checksum": "sha256:ab", "distro": "dd"},
	  "storage": {"disks": [{"select": {"match": {}}, "wipe": true}]},
	  "boot": {"strategy": "virtual_media"}}`), hw, nil, reg); err != nil {
		t.Fatalf("dd plan vMedia: %v", err)
	}
}

// The applied marker's pathway gate (docs/09 dd 重试误路径): the dd plan's
// applied report IS the terminal fact — the windows apply plan's is boot two.
// The persisted answers are the ground truth, so the gate must key on the
// plan content, not on a fresh spec read.
func TestAppliedTerminalDD(t *testing.T) {
	ddPlan := "MAMMOTH_DD_MODE=1\nMAMMOTH_DD_URL='http://x/i.img'\n"
	winApplyPlan := "MAMMOTH_WIN_MODE=apply\nMAMMOTH_WIN_INDEX=2\n"
	winSetupPlan := "MAMMOTH_UNATTEND=autounattend.xml\n"
	cases := []struct {
		name    string
		answers []string
		want    bool
	}{
		{"dd plan", []string{ddPlan}, true},
		{"dd plan among others", []string{winSetupPlan, ddPlan}, true},
		{"windows apply plan", []string{winApplyPlan}, false},
		{"windows setup plan", []string{winSetupPlan}, false},
		{"no answers", nil, false},
		{"marker is a prefix of a longer token", []string{"MAMMOTH_DD_MODE=12"}, true},
	}
	for _, c := range cases {
		ictx := &installTaskContext{}
		for _, content := range c.answers {
			ictx.Answers = append(ictx.Answers, render.AnswerFile{Name: "agent-plan.sh", Content: content})
		}
		if got := appliedTerminalDD(ictx); got != c.want {
			t.Errorf("%s: appliedTerminalDD = %v, want %v", c.name, got, c.want)
		}
	}
	// nil-safe: the gate reads the install record's context, which cleanup
	// paths parse tolerantly and may leave empty.
	if appliedTerminalDD(&installTaskContext{}) {
		t.Errorf("empty context: appliedTerminalDD = true, want false")
	}
}
