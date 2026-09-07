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
		AnswerURL:     "https://m/render/tok123/ks.cfg",
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
		AnswerURL: "https://m/render/t/ks.cfg", CompleteURL: "https://m/render/t/complete",
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
			name: "keep disk without wipe is M4 territory",
			in: render.InstallInputs{
				AnswerURL: "u", CompleteURL: "c", ImageSource: "i",
				Disks: []render.ResolvedDisk{{Device: "sda", Wipe: false}},
			},
			want: "M4",
		},
		{
			name: "preserve partition is M4 territory",
			in: render.InstallInputs{
				AnswerURL: "u", CompleteURL: "c", ImageSource: "i",
				Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
					{Mount: "/data", FS: "xfs", Preserve: true},
				}}},
			},
			want: "M4",
		},
		{
			name: "partition without size",
			in: render.InstallInputs{
				AnswerURL: "u", CompleteURL: "c", ImageSource: "i",
				Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
					{Mount: "/", FS: "xfs"},
				}}},
			},
			want: "size",
		},
		{
			name: "missing image source",
			in:   render.InstallInputs{AnswerURL: "u", CompleteURL: "c"},
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
