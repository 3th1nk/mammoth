package rocky9

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

// Golden tests for the kickstart dialect: the rendered output is the contract
// between Mammoth and Anaconda — regressions here are install failures on
// real hardware.
func TestRenderWipeStorageAndBond(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		TaskToken:     "tok123",
		MachineID:     "mch_x",
		Hostname:      "node-01",
		ImageSource:   "https://mirror.example/rocky9",
		RootPassword:  "s3creT-pw",
		SSHPublicKeys: []string{"ssh-ed25519 AAA k@h"},
		BootDrive:     "nvme0n1",
		AnswerBaseURL: "https://m/render/tok123",
		CompleteURL:   "https://m/render/tok123/complete",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/boot", FS: "xfs", SizeMB: 1024},
				{Mount: "/", FS: "xfs", Grow: true},
			}},
			{Device: "nvme1n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/data", FS: "xfs", Grow: true},
			}},
		},
		Network: []render.NetworkEntry{
			{
				Bond: &render.NetBond{
					SlavesMACs: []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"},
					Mode:       "802.3ad",
					Params:     map[string]string{"miimon": "100"},
				},
				Addresses:   []string{"172.16.1.11/24"},
				Routes:      []render.NetRoute{{To: "default", Via: "172.16.1.1"}},
				Nameservers: []string{"10.0.0.53"},
				MTU:         9000,
			},
		},
	}

	answers, boot, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(answers) != 1 || answers[0].Name != "ks.cfg" {
		t.Fatalf("want single ks.cfg, got %+v", answers)
	}
	ks := answers[0].Content

	for _, want := range []string{
		"clearpart --drives=nvme0n1,nvme1n1 --initlabel --all",
		"part /boot/efi --fstype=efi --ondisk=nvme0n1 --size=512",
		"part / --fstype=xfs --ondisk=nvme0n1 --grow",
		"bootloader --location=mbr --boot-drive=nvme0n1",
		"rootpw --plaintext s3creT-pw",
		`sshkey --username=root "ssh-ed25519 AAA k@h"`,
		"url --url=https://mirror.example/rocky9",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("kickstart missing %q", want)
		}
	}

	// Bond stanzas are generated in %pre by resolving MACs at install time.
	if !strings.Contains(ks, "iface_by_mac aa:bb:cc:dd:ee:01") ||
		!strings.Contains(ks, "iface_by_mac aa:bb:cc:dd:ee:02") {
		t.Errorf("bond slaves must resolve by MAC in the pre hook")
	}
	if !strings.Contains(ks, "--bondslaves=$bond_slaves") ||
		!strings.Contains(ks, "--bondopts=mode=802.3ad,miimon=100") {
		t.Errorf("bond stanza wrong")
	}
	if !strings.Contains(ks, "--ip=172.16.1.11 --netmask=255.255.255.0 --gateway=172.16.1.1") {
		t.Errorf("bond addressing wrong")
	}
	if !strings.Contains(ks, "--nameserver=10.0.0.53") || !strings.Contains(ks, "--mtu=9000") {
		t.Errorf("dns/mtu wrong")
	}
	if !strings.Contains(ks, "%include /run/install/mammoth/90-network.ks") {
		t.Errorf("dynamic network include missing")
	}

	// Completion callback unblocks the install stage.
	if !strings.Contains(ks, `curl -fsS -X POST -H 'Content-Type: application/json'`) ||
		!strings.Contains(ks, "https://m/render/tok123/complete") {
		t.Errorf("completion callback missing")
	}
	if !strings.Contains(ks, "hostnamectl set-hostname node-01") {
		t.Errorf("hostname missing")
	}

	if boot.KernelArgs != "inst.ks=https://m/render/tok123/ks.cfg inst.repo=cdrom inst.text" {
		t.Errorf("boot params wrong: %q", boot.KernelArgs)
	}
}

func TestRenderStaticAddressAndCIDR(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "https://mirror.example/rocky9",
		Network: []render.NetworkEntry{
			{Match: &render.NetMatch{MAC: "aa:bb:cc:dd:ee:01"}, SetName: "mgmt0",
				Addresses: []string{"10.0.1.11/24"}, Nameservers: []string{"10.0.0.53"}},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	if !strings.Contains(ks, "--interfacename=mgmt0") {
		t.Errorf("set_name missing")
	}
	if !strings.Contains(ks, "--ip=10.0.1.11 --netmask=255.255.255.0") {
		t.Errorf("cidr→netmask conversion wrong")
	}
}

func TestRenderRejectsUnrenderableInputs(t *testing.T) {
	d := New()
	cases := []struct {
		name string
		in   render.InstallInputs
		want string
	}{
		{
			name: "disk without wipe or keep",
			in: render.InstallInputs{
				AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
				Disks: []render.ResolvedDisk{{Device: "sda", Wipe: false}},
			},
			want: "declare wipe or keep",
		},
		{
			name: "preserved partition without onpart binding",
			in: render.InstallInputs{
				AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
				Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
					{Mount: "/data", FS: "xfs", Preserve: true, Number: 1},
				}}},
			},
			want: "no onpart binding",
		},
		{
			name: "partition without size",
			in: render.InstallInputs{
				AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
				Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
					{Mount: "/", FS: "xfs"},
				}}},
			},
			want: "size",
		},
		{
			name: "missing image source",
			in:   render.InstallInputs{AnswerBaseURL: "u", CompleteURL: "c"},
			want: "image source",
		},
	}
	for _, tc := range cases {
		_, _, err := d.RenderAnswers(tc.in, render.MachineView{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestPrefixToMask(t *testing.T) {
	cases := map[int]string{24: "255.255.255.0", 16: "255.255.0.0", 8: "255.0.0.0", 32: "255.255.255.255", 30: "255.255.255.252"}
	for bits, want := range cases {
		if got := prefixToMask(bits); got != want {
			t.Errorf("prefixToMask(%d) = %s, want %s", bits, got, want)
		}
	}
}

// M4: keep semantics + %pre drift guard (docs/09-roadmap.md M4, docs/06 §4).
func TestRenderKeepPartitionsAndDriftGuard(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "https://mirror.example/rocky9",
		DriftCheck:  true,
		BootDrive:   "nvme0n1",
		Disks: []render.ResolvedDisk{
			// system disk: wiped and rebuilt
			{Device: "nvme0n1", Serial: "S6XPN0001", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
			}},
			// data disk: partitions kept block-level — partition 1 preserved,
			// partition 2 (snapshot leftover) removed, rest rebuilt
			{Device: "sda", Serial: "GIM256_2021", Baseline: []render.BaselinePartition{
				{Device: "sda1", Number: 1, StartBytes: 1048576, EndBytes: 537001487, SizeBytes: 536870912, UUID: "b2a1c3d4-0000", FSType: "xfs"},
				{Device: "sda2", Number: 2, StartBytes: 537001488, EndBytes: 1073741839, SizeBytes: 536870352, UUID: "9999-8888"},
			}, Remove: []string{"sda2"}, Partitions: []render.ResolvedPartition{
				{Mount: "/data", Preserve: true, Number: 1, OnPart: "sda1", UUID: "b2a1c3d4-0000", FS: "xfs"},
				{Mount: "/extra", FS: "xfs", Grow: true},
			}},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content

	// wiped disk only in --drives; kept disk excluded
	if !strings.Contains(ks, "clearpart --drives=nvme0n1 --initlabel --all") {
		t.Errorf("wipe disk clearpart wrong")
	}
	if strings.Contains(ks, "--drives=nvme0n1,sda") || strings.Contains(ks, "--drives=sda") {
		t.Errorf("kept disk must not be wiped: clearpart drives")
	}
	// precise removal of the non-preserved leftover
	if !strings.Contains(ks, "clearpart --list=sda2") {
		t.Errorf("clearpart --list for leftovers missing")
	}
	// preserved partition: reuse unformatted via onpart
	if !strings.Contains(ks, "part /data --onpart=sda1 --noformat") {
		t.Errorf("preserve mount line missing")
	}
	// new partitions on the kept disk target it explicitly
	if !strings.Contains(ks, "part /extra --fstype=xfs --ondisk=sda --grow") {
		t.Errorf("new partition on kept disk wrong")
	}
	// drift guard pins the snapshot baseline (sysfs sectors + blkid uuid)
	for _, want := range []string{
		"_d=/sys/block/sda/sda1",
		`[ "$(cat $_d/start)" = "2048" ] || report "sda1 start drifted"`,
		`[ "$(cat $_d/size)" = "1048576" ] || report "sda1 size drifted"`,
		`[ "$(blkid -s UUID -o value /dev/sda1)" = "b2a1c3d4-0000" ] || report "sda1 uuid drifted"`,
		`{"status":"failed","detail":"LAYOUT_DRIFT: '`,
		`"$1"'"}'`,
		"https://m/render/t/complete",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("drift guard missing %q", want)
		}
	}
}

func TestRenderKeepDiskUntouched(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i", DriftCheck: true,
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/", FS: "xfs", Grow: true}}},
			{Device: "sdb", Serial: "KEEPME", KeepDisk: true},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	if strings.Contains(ks, "sdb") {
		t.Errorf("keep:disk device must not appear anywhere in the kickstart")
	}
}

func TestRenderDriftCheckToggle(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i", DriftCheck: false,
		Disks: []render.ResolvedDisk{
			{Device: "sda", Baseline: []render.BaselinePartition{
				{Device: "sda1", Number: 1, StartBytes: 1048576, SizeBytes: 536870896, UUID: "x"},
			}, Remove: []string{"sda1"}, Partitions: []render.ResolvedPartition{
				{Mount: "/data", Preserve: true, Number: 1, OnPart: "sda1", UUID: "x"},
			}},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(answers[0].Content, "LAYOUT_DRIFT") {
		t.Errorf("drift guard must be omitted when policy.verify_layout is false")
	}
	if !strings.Contains(answers[0].Content, "part /data --onpart=sda1 --noformat") {
		t.Errorf("preserve mount must survive the toggle")
	}
}

// M6: declarative software RAID renders anaconda raid lines (docs/09 M6).
func TestRenderSoftwareRaid(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "i", BootDrive: "sda",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot", FS: "xfs", SizeMB: 1024}}},
			{Device: "sdb", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot2", FS: "xfs", SizeMB: 1024}}},
		},
		Raid: []render.ResolvedRaid{
			{Name: "md0", Level: "1", Mode: "software", Members: []string{"sda", "sdb"},
				Partitions: []render.ResolvedPartition{
					{Mount: "/", FS: "xfs", Grow: true}}},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	for _, want := range []string{
		"part raid.md0-0 --size=1 --grow --ondisk=sda",
		"part raid.md0-1 --size=1 --grow --ondisk=sdb",
		"raid / --fstype=xfs --level=1 --device=md0 raid.md0-0 raid.md0-1",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("software raid missing %q", want)
		}
	}
}

// M6: software raid with multiple partitions is rejected (LVM later).
func TestRenderSoftwareRaidSinglePartition(t *testing.T) {
	d := New()
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		Raid: []render.ResolvedRaid{
			{Name: "md0", Level: "1", Mode: "software", Members: []string{"sda", "sdb"},
				Partitions: []render.ResolvedPartition{
					{Mount: "/", FS: "xfs", Grow: true},
					{Mount: "/var", FS: "xfs", SizeMB: 1024}}},
		},
	}
	if _, _, err := d.RenderAnswers(in, render.MachineView{}); err == nil ||
		!strings.Contains(err.Error(), "exactly one partition") {
		t.Fatalf("want single-partition constraint, got %v", err)
	}
}
