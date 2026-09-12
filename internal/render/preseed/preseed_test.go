package preseed

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
)

// fetchAnswers splits the rendered answer set by name.
func fetchAnswers(t *testing.T, answers []render.AnswerFile) (preseed, pre, post string) {
	t.Helper()
	for _, a := range answers {
		switch a.Name {
		case "preseed.cfg":
			preseed = a.Content
		case "run/mammoth/pre-install.sh":
			pre = a.Content
		case "run/mammoth/post-install.sh":
			post = a.Content
		}
	}
	if preseed == "" || pre == "" || post == "" {
		t.Fatalf("expected preseed.cfg + run/mammoth/{pre,post}-install.sh, got %+v", answers)
	}
	return preseed, pre, post
}

func wipeInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken:     "tokd",
		MachineID:     "mch_d",
		Hostname:      "node-d1.corp.example",
		ImageSource:   "https://mirror.example/debian-12.netinst.iso",
		RootPassword:  "dRoot-pw",
		SSHPublicKeys: []string{"ssh-ed25519 AAA d@m"},
		BootDrive:     "sda",
		AnswerBaseURL: "https://m/render/tokd",
		CompleteURL:   "https://m/render/tokd/complete",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", SizeMB: 20480},
				{Mount: "swap", FS: "swap", SizeMB: 4096},
				{Mount: "/data", FS: "ext4", Grow: true},
			}},
		},
		Network: []render.NetworkEntry{
			{
				Match:       &render.NetMatch{MAC: "aa:bb:cc:dd:ee:01"},
				Addresses:   []string{"172.16.1.11/24"},
				Routes:      []render.NetRoute{{To: "default", Via: "172.16.1.1"}},
				Nameservers: []string{"10.0.0.53", "10.0.0.54"},
				Search:      []string{"corp.example"},
			},
		},
	}
}

// The preseed dialect golden test: wipe layout with static networking, the
// offline seed contract (file=/cdrom), and the completion callback.
func TestRenderPreseedWipeAndStaticIP(t *testing.T) {
	d := New("debian12")
	in := wipeInputs()
	answers, boot, err := d.RenderAnswers(in, render.MachineView{
		ID: "mch_d", Hardware: &bmc.HardwareView{Disks: []bmc.DiskView{
			{Name: "sda", SizeBytes: 960197124096},
		}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	preseed, pre, post := fetchAnswers(t, answers)

	for _, want := range []string{
		"d-i debian-installer/locale string en_US.UTF-8",
		"d-i passwd/root-password string dRoot-pw",
		"d-i passwd/make-user boolean false",
		"d-i partman-auto/disk string /dev/sda",
		"d-i partman-partitioning/default_label string gpt",
		"d-i grub-installer/bootdev string /dev/sda",
		"d-i apt-setup/use_mirror boolean false",
		"d-i pkgsel/include string openssh-server",
		// static netcfg
		"d-i netcfg/get_ipaddress string 172.16.1.11",
		"d-i netcfg/get_netmask string 255.255.255.0",
		"d-i netcfg/get_gateway string 172.16.1.1",
		"d-i netcfg/get_nameservers string 10.0.0.53 10.0.0.54",
		"d-i netcfg/confirm_static boolean true",
		// dotted hostname splits into host + domain
		"d-i netcfg/get_hostname string node-d1",
		"d-i netcfg/get_domain string corp.example",
		// hooks ride on the boot medium
		"d-i preseed/early_command string sh /cdrom/run/mammoth/pre-install.sh",
		"d-i preseed/late_command string sh /cdrom/run/mammoth/post-install.sh",
	} {
		if !strings.Contains(preseed, want) {
			t.Errorf("preseed missing %q", want)
		}
	}
	// the kept-out pieces of the dialect
	for _, banned := range []string{"xfs", "bond", "172.16.1.12"} {
		if strings.Contains(preseed, banned) {
			t.Errorf("preseed leaked %q", banned)
		}
	}

	// completion callback + ssh access in the post script
	for _, want := range []string{
		"ssh-ed25519 AAA d@m",
		"PermitRootLogin yes",
		"--post-data='{\"status\":\"ok\"",
		"https://m/render/tokd/complete",
	} {
		if !strings.Contains(post, want) {
			t.Errorf("post-install.sh missing %q", want)
		}
	}
	if !strings.Contains(pre, `\"detail\":\"pre_install failed\"`) {
		t.Errorf("pre-install.sh failtrap missing: %s", pre)
	}

	if boot.AnswerURL != "https://m/render/tokd/preseed.cfg" {
		t.Errorf("answer url wrong: %q", boot.AnswerURL)
	}
	if boot.KernelArgs != "auto=true priority=critical file=/cdrom/preseed.cfg "+
		"debian-installer/locale=en_US.UTF-8 keyboard-configuration/layoutcode=us "+
		"console-setup/ask_detect=false console-setup/layoutcode=us" {
		t.Errorf("boot params wrong: %q", boot.KernelArgs)
	}
}

// expert_recipe: declaration order is on-disk order; efi/swap methods carry
// no filesystem/mountpoint lines; grow maxes out at the partman clamp.
func TestExpertRecipe(t *testing.T) {
	tgt := target{device: "sda", partitions: []render.ResolvedPartition{
		{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
		{Mount: "/", FS: "ext4", SizeMB: 20480},
		{Mount: "swap", FS: "swap", SizeMB: 4096},
		{Mount: "/data", FS: "ext4", Grow: true},
	}}
	r, err := expertRecipe(tgt)
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	if !strings.HasPrefix(r, "  mammoth :: \\\n") {
		t.Errorf("recipe missing header: %q", r)
	}
	if strings.HasSuffix(r, "\\\n") {
		t.Errorf("recipe must not end with a continuation: %q", r)
	}
	order := []string{
		"512 512 512 free", "method{ efi } format{ }",
		"20480 20480 20480 ext4", "filesystem{ ext4 }", "mountpoint{ / }",
		"4096 4096 4096 linux-swap", "method{ swap } format{ }",
		"1 1 1000000000 ext4", "mountpoint{ /data }",
	}
	last := 0
	for _, want := range order {
		i := strings.Index(r[last:], want)
		if i < 0 {
			t.Errorf("recipe missing %q in:\n%s", want, r)
			continue
		}
		last += i
	}
	if strings.Contains(r, "mountpoint{ /boot/efi }") {
		t.Errorf("efi method must not carry a mountpoint: %s", r)
	}
}

// keep: disk on a partial distro: the disk never becomes a partman target.
func TestRenderKeepDiskPartial(t *testing.T) {
	in := wipeInputs()
	in.Disks = append(in.Disks, render.ResolvedDisk{Device: "sdb", KeepDisk: true})
	answers, _, err := New("debian12").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	preseed, _, _ := fetchAnswers(t, answers)
	if strings.Contains(preseed, "sdb") {
		t.Errorf("keep:disk device must be absent from the preseed: %s", preseed)
	}
}

func TestRenderRejectKeepPartitions(t *testing.T) {
	cases := map[string]func(*render.ResolvedDisk){
		"baseline": func(d *render.ResolvedDisk) {
			d.Baseline = []render.BaselinePartition{{Device: "sda1", Number: 1, StartBytes: 1048576, SizeBytes: 536870912}}
		},
		"remove": func(d *render.ResolvedDisk) { d.Remove = []string{"sda2"} },
		"preserve": func(d *render.ResolvedDisk) {
			d.Partitions = []render.ResolvedPartition{{Mount: "/data", Preserve: true, Number: 2, OnPart: "sda2"}}
		},
	}
	for name, seed := range cases {
		in := render.InstallInputs{
			AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
			Disks: []render.ResolvedDisk{{Device: "sda"}},
		}
		seed(&in.Disks[0])
		_, _, err := New("debian12").RenderAnswers(in, render.MachineView{})
		if err == nil || !strings.Contains(err.Error(), "keep: partitions is not supported") {
			t.Errorf("%s: want keep:partitions rejection, got %v", name, err)
		}
	}
}

func TestRenderRejects(t *testing.T) {
	base := func() render.InstallInputs {
		in := wipeInputs()
		in.Disks = []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
			{Mount: "/", FS: "ext4", Grow: true}}}}
		in.Network = nil
		return in
	}
	cases := []struct {
		name string
		mut  func(*render.InstallInputs)
		want string
	}{
		{"software raid", func(in *render.InstallInputs) {
			in.Raid = []render.ResolvedRaid{{Name: "r0", Mode: "software", Level: "1", Members: []string{"sda", "sdb"}}}
		}, "software RAID is not supported"},
		{"bond", func(in *render.InstallInputs) {
			in.Network = []render.NetworkEntry{{Bond: &render.NetBond{
				SlavesMACs: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, Mode: "active-backup"}}}
		}, "bonding is not supported by netcfg"},
		{"vlan", func(in *render.InstallInputs) {
			in.Network = []render.NetworkEntry{{VLAN: &render.NetVLAN{ID: 100, Link: "eth0"}}}
		}, "vlan is not supported by netcfg"},
		{"two static entries", func(in *render.InstallInputs) {
			in.Network = []render.NetworkEntry{
				{Addresses: []string{"172.16.1.11/24"}},
				{Addresses: []string{"198.51.100.11/24"}},
			}
		}, "only one static network entry is supported"},
		{"two install targets", func(in *render.InstallInputs) {
			in.Disks = append(in.Disks, render.ResolvedDisk{Device: "sdb", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/", FS: "ext4", Grow: true}}})
		}, "only one install target disk is supported"},
		{"unsupported fs", func(in *render.InstallInputs) {
			in.Disks[0].Partitions = []render.ResolvedPartition{{Mount: "/", FS: "xfs", Grow: true}}
		}, "filesystem xfs is not available"},
		{"two grow", func(in *render.InstallInputs) {
			in.Disks[0].Partitions = []render.ResolvedPartition{
				{Mount: "/", FS: "ext4", Grow: true},
				{Mount: "/data", FS: "ext4", Grow: true}}
		}, "only one grow partition per disk is supported"},
		{"non-kernel device", func(in *render.InstallInputs) {
			in.Disks[0].Device = "LogicalDrive1"
		}, "is not a kernel device name"},
		{"no partitions", func(in *render.InstallInputs) {
			in.Disks[0].Partitions = nil
		}, "declares no partitions"},
	}
	for _, c := range cases {
		in := base()
		c.mut(&in)
		_, _, err := New("debian12").RenderAnswers(in, render.MachineView{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
}

// A bound hardware RAID volume is a first-class install target.
func TestRenderHardwareRaidTarget(t *testing.T) {
	in := wipeInputs()
	in.Disks = []render.ResolvedDisk{{Device: "sdb", KeepDisk: true}}
	in.Raid = []render.ResolvedRaid{{
		Name: "vol0", Mode: "hardware", Level: "1", BoundDevice: "sda",
		SizeBytes: 536870912000, MemberSerials: []string{"S1", "S2"},
		Partitions: []render.ResolvedPartition{
			{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
			{Mount: "/", FS: "ext4", Grow: true},
		},
	}}
	answers, _, err := New("debian12").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	preseed, _, _ := fetchAnswers(t, answers)
	if !strings.Contains(preseed, "d-i partman-auto/disk string /dev/sda") {
		t.Errorf("raid volume not the partman target: %s", preseed)
	}
}

// One driver per registered distro name.
func TestMultiDistro(t *testing.T) {
	if New("debian12").KeepPartitionSupport() != render.SupportPartial {
		t.Errorf("keep support wrong: %v", New("debian12").KeepPartitionSupport())
	}
	for _, name := range []string{"debian12"} {
		d := New(name)
		if d.Distro() != name {
			t.Errorf("distro wrong: %q", d.Distro())
		}
		if _, _, err := d.RenderAnswers(wipeInputs(), render.MachineView{}); err != nil {
			t.Errorf("%s: render: %v", name, err)
		}
	}
}
