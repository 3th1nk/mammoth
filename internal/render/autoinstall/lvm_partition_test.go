package autoinstall

// Partition-carve PV rendering (docs/04-install-spec.md §5.1.1): a fresh VG
// whose PV lands on a disks[]-claimed holder rides the holder's remaining
// space as a raw trailing partition — the single-disk layout (ESP + PV
// partition on one drive, root on the LV). The volgroup consumes the raw
// partition's curtin id.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
)

func jsonCompact(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal config entry: %v", err)
	}
	return string(raw)
}

func TestRenderPartitionCarvePV(t *testing.T) {
	d := New("ubuntu24")
	in := render.InstallInputs{
		TaskToken:     "tokL",
		MachineID:     "mch_l",
		Hostname:      "lvm-node",
		ImageSource:   "https://mirror.example/ubuntu-24.04.iso",
		RootPassword:  "lRoot-pw",
		BootDrive:     "sda",
		AnswerBaseURL: "https://m/render/tokL",
		CompleteURL:   "https://m/render/tokL/complete",
		Disks: []render.ResolvedDisk{{
			Device: "sda", Wipe: true, SizeBytes: 3725 << 30,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 1024, Flags: []string{"esp"}},
			},
		}},
		Lvm: []render.ResolvedVolgroup{{
			Name: "vg0",
			PVs:  []render.ResolvedPV{{Device: "sda", PartitionNumber: 2, SizeBytes: 3725 << 30}},
			Logvols: []render.ResolvedLogvol{
				{Name: "root", FS: "ext4", Mount: "/", Grow: true},
				{Name: "swap", FS: "swap", Mount: "swap", SizeMB: 8192},
			},
		}},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{
		ID: "mch_l", Hardware: &bmc.HardwareView{Disks: []bmc.DiskView{{Name: "sda", SizeBytes: 3725 << 30}}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, ud := fetchUser(t, answers)
	auto := ud["autoinstall"].(map[string]any)
	storage := auto["storage"].(map[string]any)
	config := storage["config"].([]any)

	body := dumpConfig(t, config)

	// The holder disk boots (ESP carries grub_device) and the raw PV
	// partition exists with no format/mount entries.
	for _, want := range []string{
		`"id":"part-disk-sda-1"`, // ESP
		`"grub_device":true`,
		`"id":"part-disk-sda-2"`, // the carve PV — raw partition
		`"id":"vg-vg0"`,
		`"devices":["part-disk-sda-2"]`,
		`"id":"lv-vg0-root"`,
		`"id":"lv-vg0-swap"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("storage config missing %q:\n%s", want, body)
		}
	}
	// The carve partition must NOT be formatted: fmt-disk-sda-2 / mnt would
	// mean a filesystem raced onto the PV.
	for _, banned := range []string{`"id":"fmt-disk-sda-2"`, `"id":"mnt-disk-sda-2"`} {
		if strings.Contains(body, banned) {
			t.Errorf("carve PV got a format/mount entry (%s):\n%s", banned, body)
		}
	}
}

func dumpConfig(t *testing.T, config []any) string {
	t.Helper()
	var b strings.Builder
	for _, e := range config {
		b.WriteString(jsonCompact(t, e))
		b.WriteString("\n")
	}
	return b.String()
}
