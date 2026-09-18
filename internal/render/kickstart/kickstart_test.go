package kickstart

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

// Golden tests for the kickstart dialect: the rendered output is the contract
// between Mammoth and Anaconda — regressions here are install failures on
// real hardware.
func TestRenderWipeStorageAndBond(t *testing.T) {
	d := New("rocky9")
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
		"bootloader --boot-drive=nvme0n1",
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
	if !strings.Contains(ks, "network --hostname=node-01") ||
		!strings.Contains(ks, "echo node-01 > /etc/hostname") {
		t.Errorf("hostname missing")
	}

	if boot.KernelArgs != "ip=dhcp inst.ks=https://m/render/tok123/ks.cfg inst.repo=https://mirror.example/rocky9 inst.text" {
		t.Errorf("boot params wrong: %q", boot.KernelArgs)
	}
}

// UOS's customized anaconda crashes in the Finish task group under the
// text frontend (max() arg is an empty sequence — reproduced in qemu,
// scripts/uos-dev/): uniontechos must render graphical and must not carry
// inst.text on the kernel command line, while the rest of the family keeps
// text. This is the regression pin for the uniontechos unlock.
func TestRenderUniontechosRunsGraphical(t *testing.T) {
	d := New("uniontechos")
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "file:///run/install/repo", BootDrive: "sda",
		Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, SizeBytes: 214748364800,
			Partitions: []render.ResolvedPartition{
				{Mount: "/", FS: "xfs", Grow: true},
			}}},
	}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	if strings.Contains(boot.KernelArgs, "inst.text") {
		t.Errorf("uniontechos must not boot with inst.text (text frontend crashes the Finish group): %q", boot.KernelArgs)
	}
	if !strings.Contains(ks, "\ngraphical\n") {
		t.Errorf("uniontechos kickstart must run graphical: %q", ks[:120])
	}
	if strings.Contains(ks, "\ntext\n") {
		t.Errorf("uniontechos kickstart must not carry the text directive")
	}
	// No swap declared: UOS's customized bootloader module crashes the
	// Finish task group on `max(swap_devices)` over an empty sequence, so
	// the driver must append one — pinned to the boot disk and inside the
	// grow budget (an unpinned line outside the budget pushed the total
	// request past the disk on the real 2288H: "Unable to allocate
	// requested partition scheme").
	if !strings.Contains(ks, "part swap --fstype=swap --ondisk=sda --size=2048") {
		t.Errorf("uniontechos without a declared swap must get one appended to the boot disk")
	}
	// The grow partition's explicit size must have shrunk by the swap.
	if !strings.Contains(ks, "part / --fstype=xfs --ondisk=sda --size=202240") {
		t.Errorf("grow partition must carry the explicit size with the 2G swap deducted from the budget")
	}

	// A spec that declares its own swap is honored as-is — no second one.
	in2 := in
	in2.Disks = []render.ResolvedDisk{{Device: "sda", Wipe: true,
		Partitions: []render.ResolvedPartition{
			{Mount: "/", FS: "xfs", Grow: true},
			{FS: "swap", SizeMB: 4096},
		}}}
	answers2, _, err := d.RenderAnswers(in2, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render with declared swap: %v", err)
	}
	if got := strings.Count(answers2[0].Content, "--fstype=swap"); got != 1 {
		t.Errorf("declared swap must be honored exactly once, got %d", got)
	}
}

func TestRenderStaticAddressAndCIDR(t *testing.T) {
	d := New("rocky9")
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
	d := New("rocky9")
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
	d := New("rocky9")
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
	d := New("rocky9")
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
	d := New("rocky9")
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
	d := New("rocky9")
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
	d := New("rocky9")
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

// Static spec network entries must become pinned dracut early-network args —
// the remote kickstart fetch needs network before anaconda runs (real-hardware
// finding: without ip= the installer never fetched the answer file).
func TestEarlyNetworkArgsFromSpec(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		Network: []render.NetworkEntry{
			{Match: &render.NetMatch{MAC: "02:00:00:00:00:00"},
				Addresses:   []string{"198.51.100.170/24"},
				Routes:      []render.NetRoute{{To: "0.0.0.0/0", Via: "198.51.100.1"}},
				Nameservers: []string{"223.5.5.5"}},
		},
	}
	_, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatal(err)
	}
	want := "ifname=m0:02:00:00:00:00:00 ip=198.51.100.170::198.51.100.1:255.255.255.0::m0:none nameserver=223.5.5.5"
	if !strings.HasPrefix(boot.KernelArgs, want+" ") {
		t.Errorf("early net args missing: %q", boot.KernelArgs)
	}
	if !strings.Contains(boot.KernelArgs, "inst.ks=u/ks.cfg") {
		t.Errorf("inst.ks lost: %q", boot.KernelArgs)
	}
}

// nfs:// image source becomes an anaconda NFS ISO repo (host:/path, colon
// required) — an HTTP ISO file is not a valid repo (anaconda fetches only
// unpacked trees over HTTP; real-hardware finding).
func TestNFSSourceRendersNFSIsoRepo(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c",
		ImageSource: "nfs://198.51.100.248/data/os_iso/Rocky-9.7-x86_64-minimal.iso",
	}
	_, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatal(err)
	}
	want := "inst.repo=nfs:198.51.100.248:/data/os_iso/Rocky-9.7-x86_64-minimal.iso"
	if !strings.Contains(boot.KernelArgs, want) {
		t.Errorf("nfs iso repo wrong: %q", boot.KernelArgs)
	}
}

// Redfish logical-drive names ("LogicalDrive1") are not installer device
// names — wipe stanzas must move into a %pre-generated include that resolves
// the real device by size/serial (real-hardware finding: clearpart aborted
// with "Disk LogicalDrive1 does not exist").
func TestDynamicStorageResolution(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		BootDrive: "LogicalDrive1",
		Disks: []render.ResolvedDisk{{
			Device: "LogicalDrive1", SizeBytes: 3997823926272, Wipe: true,
			Partitions: []render.ResolvedPartition{{Mount: "/", FS: "xfs", Grow: true}},
		}},
	}
	ks, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatal(err)
	}
	body := ks[0].Content
	for _, want := range []string{
		"90-storage.ks",
		"D0=$(resolve 3997823926272 '' '')",
		"clearpart --drives=$D0 --initlabel --all",
		// explicit size (capacity − margin) inside the dynamic include too
		"part / --fstype=xfs --ondisk=$D0 --size=3812110",
		"bootloader --boot-drive=$D0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dynamic storage missing %q", want)
		}
	}
	// 静态区不得再出现 Redfish 盘名或重复的 bootloader/clearpart
	if strings.Contains(body, "LogicalDrive1") {
		t.Errorf("static ks leaks Redfish device name")
	}
	if strings.Count(body, "bootloader --boot-drive") != 1 {
		t.Errorf("bootloader must appear exactly once (in the include)")
	}
	if boot.AnswerURL == "" {
		t.Errorf("boot params lost")
	}
}

// Hostname must land via the native network --hostname command (anaconda
// writes /etc/hostname) plus a %post file write — hostnamectl is unreliable
// from the %post chroot (real-hardware finding: hostname stayed
// localhost.localdomain).
func TestHostnameRendered(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i", Hostname: "hw-real",
	}
	ks, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatal(err)
	}
	body := ks[0].Content
	if !strings.Contains(body, "network --hostname=hw-real") {
		t.Errorf("native network --hostname missing")
	}
	if !strings.Contains(body, "echo hw-real > /etc/hostname") {
		t.Errorf("post hostname fallback missing")
	}
}

// %pre/%post failures must reach the completion endpoint with the failing
// phase — the task error then shows the reason, not an opaque timeout.
func TestFailTrapRendered(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		AnswerBaseURL: "http://m/render/tok", CompleteURL: "http://m/render/tok/complete",
		ImageSource: "i", Hostname: "hw",
		Network: []render.NetworkEntry{{Match: &render.NetMatch{MAC: "aa:bb:cc:dd:ee:01"}}},
	}
	ks, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatal(err)
	}
	body := ks[0].Content
	for _, want := range []string{
		`detail\":\"network pre_install failed\"`,
		`detail\":\"post_install script failed\"`,
		"|| true' ERR",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fail trap missing %q", want)
		}
	}
}

// Regression (Huawei 2288H V5): blivet clamped --grow at the 2^32 sector
// boundary on a controller volume (3.6T disk, root stopped at 2TiB, --maxsize
// ignored too) — a grow partition on a disk with known inventory size renders
// as an explicit --size (capacity − fixed − 512MB margin), not --grow. And
// the hostname rides the FIRST resolved network stanza (a bare
// `network --hostname=` line is not applied by anaconda on every path).
func TestRenderGrowExplicitSizeAndHostnameOnStanza(t *testing.T) {
	in := render.InstallInputs{
		TaskToken:     "tokm",
		MachineID:     "mch_m",
		Hostname:      "rk9-host",
		RootPassword:  "pw",
		AnswerBaseURL: "https://m/render/tokm",
		CompleteURL:   "https://m/render/tokm/complete",
		ImageSource:   "https://mirror.example/rocky9",
		Disks: []render.ResolvedDisk{
			{Device: "sda", SizeBytes: 3999999721472, Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", Grow: true}}},
		},
		Network: []render.NetworkEntry{
			{Match: &render.NetMatch{MAC: "aa:bb:cc:dd:ee:01"},
				Addresses: []string{"198.51.100.170/24"}},
		},
	}
	answers, _, err := New("rocky9").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	for _, want := range []string{
		"part / --fstype=ext4 --ondisk=sda --size=3813673",
		"--hostname=rk9-host --activate",
		"findmnt -nro SOURCE /mnt/sysimage",
		"sfdisk --no-reread --force -N \"$root_num\" \"/dev/$root_disk\"",
		"resize2fs \"$root_src\"",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("kickstart missing %q", want)
		}
	}
	if strings.Contains(ks, "--grow") || strings.Contains(ks, "--maxsize") {
		t.Errorf("known inventory size must render an explicit partition size, not --grow/--maxsize")
	}
	if strings.Count(ks, "--hostname=rk9-host") != 2 {
		// stanza + the standalone `network --hostname=` line
		t.Errorf("hostname should appear on the stanza and the standalone line: %d", strings.Count(ks, "--hostname=rk9-host"))
	}
}

// grow partition sizing paths: explicit render-time size when the inventory
// capacity is known (the blivet grow-clamp workaround), --grow fallback when
// the budget cannot be trusted (no size, preserved/unsized siblings).
func TestRenderGrowExplicitSizePaths(t *testing.T) {
	run := func(t *testing.T, disks []render.ResolvedDisk) string {
		t.Helper()
		in := render.InstallInputs{
			TaskToken: "tok", MachineID: "mch_g",
			AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
			ImageSource: "i", Disks: disks,
		}
		answers, _, err := New("rocky9").RenderAnswers(in, render.MachineView{})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return answers[0].Content
	}
	gib := int64(10) * 1024 * 1024 * 1024

	t.Run("known capacity", func(t *testing.T) {
		ks := run(t, []render.ResolvedDisk{{Device: "sda", SizeBytes: gib, Wipe: true,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/boot", FS: "xfs", SizeMB: 1024},
				{Mount: "/", FS: "xfs", Grow: true}}}})
		// 10240 − 512 − 1024 − 512(margin) = 8192
		if !strings.Contains(ks, "part / --fstype=xfs --ondisk=sda --size=8192") {
			t.Errorf("explicit grow size missing")
		}
		if strings.Contains(ks, "--grow") {
			t.Errorf("known capacity must not fall back to --grow")
		}
	})

	t.Run("multiple grows split the rest", func(t *testing.T) {
		ks := run(t, []render.ResolvedDisk{{Device: "sda", SizeBytes: gib, Wipe: true,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
				{Mount: "/data", FS: "xfs", Grow: true}}}})
		// (10240 − 512 − 512) / 2 = 4608 each
		if !strings.Contains(ks, "part / --fstype=xfs --ondisk=sda --size=4608") ||
			!strings.Contains(ks, "part /data --fstype=xfs --ondisk=sda --size=4608") {
			t.Errorf("grow split across partitions missing")
		}
	})

	t.Run("unknown capacity falls back to --grow", func(t *testing.T) {
		ks := run(t, []render.ResolvedDisk{{Device: "sda", Wipe: true,
			Partitions: []render.ResolvedPartition{{Mount: "/", FS: "xfs", Grow: true}}}})
		if !strings.Contains(ks, "part / --fstype=xfs --ondisk=sda --grow") {
			t.Errorf("--grow fallback missing")
		}
	})

	t.Run("preserved sibling invalidates the budget", func(t *testing.T) {
		ks := run(t, []render.ResolvedDisk{{Device: "sda", SizeBytes: gib,
			Baseline: []render.BaselinePartition{
				{Device: "sda1", Number: 1, StartBytes: 1048576, SizeBytes: 536870912, UUID: "u"}},
			Partitions: []render.ResolvedPartition{
				{Mount: "/data", Preserve: true, Number: 1, OnPart: "sda1", UUID: "u", FS: "xfs"},
				{Mount: "/extra", FS: "xfs", Grow: true}}}})
		if !strings.Contains(ks, "part /extra --fstype=xfs --ondisk=sda --grow") {
			t.Errorf("preserve sibling must fall back to --grow")
		}
	})
}

// TestFirmwareSupportDeclarations pins the media firmware range per distro:
// rocky10 dropped BIOS boot images upstream (UEFI-only, docs/compat/distros.md)
// — the submit-time firmware gate keys off exactly this declaration.
func TestFirmwareSupportDeclarations(t *testing.T) {
	cases := map[string]render.FirmwareSupport{
		"rocky9":      render.FirmwareAll,
		"rocky10":     render.FirmwareUEFIOnly,
		"centos7":     render.FirmwareAll,
		"kylinv10":    render.FirmwareAll,
		"kylinv11":    render.FirmwareAll,
		"uniontechos": render.FirmwareAll,
	}
	for distro, want := range cases {
		if got := New(distro).FirmwareSupport(); got != want {
			t.Errorf("%s firmware support = %q, want %q", distro, got, want)
		}
	}
}
