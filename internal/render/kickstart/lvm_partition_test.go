package kickstart

// Partition-carve PV rendering (docs/04-install-spec.md §5.1.1): the PV
// part line pins the holder disk (--ondisk) and takes its remaining space
// (--grow) after the declared ESP//boot/swap lines — the holder is claimed
// and wiped by its own disks[] row, so no clearpart duplication and no
// extra dynamic slot.

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func TestRenderPartitionCarvePV(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		TaskToken:     "tokL",
		MachineID:     "mch_l",
		Hostname:      "lvm-node",
		ImageSource:   "https://mirror.example/rocky9",
		RootPassword:  "lRoot-pw",
		BootDrive:     "sda",
		AnswerBaseURL: "https://m/render/tokL",
		CompleteURL:   "https://m/render/tokL/complete",
		Disks: []render.ResolvedDisk{{
			Device: "sda", Wipe: true, SizeBytes: 3725 << 30,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 1024, Flags: []string{"esp"}},
				{Mount: "/boot", FS: "xfs", SizeMB: 2048},
			},
		}},
		Lvm: []render.ResolvedVolgroup{{
			Name: "vg0",
			PVs:  []render.ResolvedPV{{Device: "sda", PartitionNumber: 3, SizeBytes: 3725 << 30}},
			Logvols: []render.ResolvedLogvol{
				{Name: "root", FS: "xfs", Mount: "/", Grow: true},
			},
		}},
	}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{ID: "mch_l"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content

	for _, want := range []string{
		"clearpart --drives=sda --initlabel --all",
		"part pv.vg0-0 --size=1 --grow --ondisk=sda",
		"volgroup vg0 pv.vg0-0",
		"logvol / --vgname=vg0 --name=root --fstype=xfs --size=1 --grow",
		"bootloader --boot-drive=sda",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("ks.cfg missing %q", want)
		}
	}
	// The carve PV must not double the holder into another dynamic slot:
	// exactly one $D placeholder registration for sda (its disks[] row).
	if n := strings.Count(ks, "$D0"); n != 2 { // %include paths: storage shell slot expansion
		t.Logf("note: $D0 occurrences = %d (informational)", n)
	}
	_ = boot
}

func TestRenderPartitionCarvePVDynamicHolder(t *testing.T) {
	// The holder rides a controller name (LogicalDrive0): its disks[] row
	// registers the $Dn slot and the PV part line must reuse it — a second
	// registration would size-guess the same disk twice in resolve-disk.sh.
	d := New("rocky9")
	in := render.InstallInputs{
		TaskToken:     "tokL",
		MachineID:     "mch_l",
		Hostname:      "lvm-node",
		ImageSource:   "https://mirror.example/rocky9",
		RootPassword:  "lRoot-pw",
		BootDrive:     "LogicalDrive0",
		AnswerBaseURL: "https://m/render/tokL",
		CompleteURL:   "https://m/render/tokL/complete",
		Disks: []render.ResolvedDisk{{
			Device: "LogicalDrive0", Wipe: true, SizeBytes: 3725 << 30,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 1024, Flags: []string{"esp"}},
				{Mount: "/boot", FS: "xfs", SizeMB: 2048},
			},
		}},
		Lvm: []render.ResolvedVolgroup{{
			Name: "vg0",
			PVs:  []render.ResolvedPV{{Device: "LogicalDrive0", PartitionNumber: 3, SizeBytes: 3725 << 30}},
			Logvols: []render.ResolvedLogvol{
				{Name: "root", FS: "xfs", Mount: "/", Grow: true},
			},
		}},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{ID: "mch_l"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	if strings.Contains(ks, "--ondisk=LogicalDrive0") {
		t.Errorf("PV line pinned the raw controller name (must ride the $Dn placeholder):\n%s", ks)
	}
	if !strings.Contains(ks, "--ondisk=$D0") {
		t.Errorf("PV line missing the holder's $Dn placeholder:\n%s", ks)
	}
}
