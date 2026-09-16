package autoinstall

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
)

// fetchUser decodes the emitted user-data JSON (cloud-init accepts JSON
// user-data; the golden assertions read it structurally).
func fetchUser(t *testing.T, answers []render.AnswerFile) (meta string, ud map[string]any) {
	t.Helper()
	for _, a := range answers {
		switch a.Name {
		case "meta-data":
			meta = a.Content
		case "user-data":
			raw := strings.TrimPrefix(a.Content, "#cloud-config\n")
			if err := json.Unmarshal([]byte(raw), &ud); err != nil {
				t.Fatalf("user-data json: %v\n%s", err, a.Content)
			}
		}
	}
	if ud == nil {
		t.Fatal("autoinstall.yaml missing")
	}
	return meta, ud
}

// The autoinstall dialect golden test: seed structure, netplan bond by MAC
// (the batch-stable selector), curtin storage for the wipe path, and the
// completion callback (docs/06-install-pipeline.md §5 matrix: partial).
func TestRenderAutoinstallWipeAndBond(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		TaskToken:     "tok9",
		MachineID:     "mch_u",
		Hostname:      "node-u1",
		ImageSource:   "https://mirror.example/ubuntu-22.04.iso",
		RootPassword:  "uRoot-pw",
		SSHPublicKeys: []string{"ssh-ed25519 AAA u@m"},
		BootDrive:     "nvme0n1",
		AnswerBaseURL: "https://m/render/tok9",
		CompleteURL:   "https://m/render/tok9/complete",
		DriftCheck:    false,
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
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
			},
		},
		Scripts: []render.ScriptEntry{
			{Stage: "post_install", Inline: "echo u-done"},
		},
	}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{
		ID: "mch_u", Hardware: &bmc.HardwareView{Disks: []bmc.DiskView{
			{Name: "nvme0n1", SizeBytes: 1920383410176},
		}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(answers) != 2 {
		t.Fatalf("nocloud seed = meta-data + user-data, got %+v", answers)
	}
	meta, ud := fetchUser(t, answers)
	_ = meta // nocloud meta-data must exist but carries no content

	auto, _ := ud["autoinstall"].(map[string]any)
	if auto == nil {
		t.Fatal("autoinstall section missing")
	}

	// access: root password + ssh keys are set during install (late-commands)
	// — anything left to first-boot cloud-init silently no-ops because the
	// booted system cannot read the seed (real-hardware lesson).
	if ud["chpasswd"] != nil {
		t.Errorf("chpasswd must not ride in user-data anymore (first-boot cannot read it)")
	}
	ssh, _ := auto["ssh"].(map[string]any)
	if _, ok := ssh["authorized-keys"]; ok {
		t.Errorf("authorized-keys must move to late-commands")
	}
	if auto["shutdown"] != "reboot" {
		t.Errorf("shutdown: reboot missing")
	}

	// netplan: bond slaves matched by MAC address natively
	net := auto["network"].(map[string]any)
	bonds := net["bonds"].(map[string]any)
	bond0 := bonds["bond0"].(map[string]any)
	ifaces := bond0["interfaces"].([]any)
	if len(ifaces) != 2 {
		t.Fatalf("bond slaves wrong: %v", ifaces)
	}
	eths := net["ethernets"].(map[string]any)
	for _, id := range ifaces {
		e := eths[id.(string)].(map[string]any)
		mac := e["match"].(map[string]any)["macaddress"]
		if mac != "aa:bb:cc:dd:ee:01" && mac != "aa:bb:cc:dd:ee:02" {
			t.Errorf("slave %v matches wrong mac %v", id, mac)
		}
	}
	if bond0["gateway4"] != "172.16.1.1" {
		t.Errorf("bond gateway wrong: %v", bond0["gateway4"])
	}

	// curtin storage: disk→partition→format→mount for the wiped disk
	st := auto["storage"].(map[string]any)
	config := st["config"].([]any)
	joined := ""
	for _, c := range config {
		b, _ := json.Marshal(c)
		joined += string(b) + "\n"
	}
	for _, want := range []string{
		`"path":"/dev/nvme0n1"`, `"ptable":"gpt"`, `"wipe":"superblock"`,
		`"fstype":"fat32"`, `"path":"/boot/efi"`, `"path":"/"`, `"grub_device":true`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("curtin storage missing %q", want)
		}
	}
	if strings.Contains(joined, "sda") {
		t.Errorf("no sda in this spec, storage leaked: %s", joined)
	}

	// completion callback + user script + hostname + access provisioning
	late := strings.Join(toStrSlice(auto["late-commands"]), "\n")
	if !strings.Contains(late, "https://m/render/tok9/complete") {
		t.Errorf("completion callback missing")
	}
	if !strings.Contains(late, "python3") {
		t.Errorf("completion callback must use python3 (curl absent in subiquity env)")
	}
	if !strings.Contains(late, `chpasswd`) || !strings.Contains(late, "uRoot-pw") {
		t.Errorf("root password provisioning missing")
	}
	if !strings.Contains(late, "PermitRootLogin yes") {
		t.Errorf("PermitRootLogin provisioning missing")
	}
	if !strings.Contains(late, "ssh-ed25519 AAA u@m") {
		t.Errorf("ssh key provisioning missing")
	}
	if !strings.Contains(late, "echo u-done") {
		t.Errorf("user post script missing")
	}
	if !strings.Contains(late, "node-u1") {
		t.Errorf("hostname missing")
	}

	if boot.KernelArgs != "autoinstall ds=nocloud-net;s=file:///cdrom/" {
		t.Errorf("boot params wrong: %q", boot.KernelArgs)
	}
}

// BIOS + GPT: a declared biosgrub partition renders as a raw bios_grub
// partition (curtin refuses bootloader install without an explicit one).
func TestRenderBiosGrubPartition(t *testing.T) {
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
				{FS: "fat32", SizeMB: 1, Flags: []string{"biosgrub"}},
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", Grow: true}}},
		},
	}
	_, ud, err := func() ([]render.AnswerFile, map[string]any, error) {
		answers, _, err := New("ubuntu22").RenderAnswers(in, render.MachineView{})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		_, u := fetchUser(t, answers)
		return nil, u, nil
	}()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	st, _ := json.Marshal(ud["autoinstall"].(map[string]any)["storage"])
	if !strings.Contains(string(st), `"flag":"bios_grub"`) {
		t.Errorf("bios_grub partition missing from curtin config: %s", st)
	}
}

// keep: disk on a partial distro: the disk is absent from the curtin config.
func TestRenderKeepDiskPartial(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "i",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/", FS: "xfs", Grow: true}}},
			{Device: "sdb", KeepDisk: true},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, ud := fetchUser(t, answers)
	b, _ := json.Marshal(ud["autoinstall"].(map[string]any)["storage"])
	if strings.Contains(string(b), "sdb") {
		t.Errorf("keep:disk device must be absent from curtin config: %s", b)
	}
}

// keep: partitions on a partial distro is rejected at render (defense in
// depth behind the submit gate).
func TestRenderRejectsKeepPartitions(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Baseline: []render.BaselinePartition{
				{Device: "sda1", Number: 1, StartBytes: 1048576, SizeBytes: 536870912},
			}, Partitions: []render.ResolvedPartition{
				{Mount: "/data", Preserve: true, Number: 1, OnPart: "sda1"},
			}},
		},
	}
	_, _, err := d.RenderAnswers(in, render.MachineView{})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("want keep:partitions rejection, got %v", err)
	}
}

func toStrSlice(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// The PXE variant: casper mounts the live root over NFS from the unpacked
// tree (no whole-ISO-into-RAM), and the nocloud seed rides HTTP. Static-net
// declarations are rejected — netplan cannot apply before the NFS root is up.
func TestRenderNetbootCasperArgs(t *testing.T) {
	d := New("ubuntu22")
	base := render.InstallInputs{
		TaskToken:     "toku",
		MachineID:     "mch_u",
		Hostname:      "node-u1",
		ImageSource:   "https://mirror.example/ubuntu-22.04.5-live-server-amd64.iso",
		RootPassword:  "uRoot-pw",
		BootDrive:     "sda",
		AnswerBaseURL: "http://10.0.0.1:8080/render/toku",
		CompleteURL:   "http://10.0.0.1:8080/render/toku/complete",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", SizeMB: 20480},
			}},
		},
	}
	in := base
	in.Netboot = &render.NetbootInputs{
		PoolURL:    "http://10.0.0.1:8080/netboot/files/toku",
		NFSRootURL: "10.0.0.1:/export/netboot/toku/iso",
	}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if boot.NetbootKernelArgs == "" ||
		!strings.Contains(boot.NetbootKernelArgs, "ds=nocloud-net;s=http://10.0.0.1:8080/render/toku/") ||
		!strings.Contains(boot.NetbootKernelArgs, "boot=casper netboot=nfs nfsroot=10.0.0.1:/export/netboot/toku/iso") {
		t.Errorf("netboot args missing casper/nfsroot/seed: %q", boot.NetbootKernelArgs)
	}
	// The seed files stay identical to the ISO path (ds= is an absolute URL).
	var found bool
	for _, a := range answers {
		if a.Name == "user-data" && strings.Contains(a.Content, "autoinstall") {
			found = true
		}
	}
	if !found {
		t.Errorf("user-data missing from netboot render: %+v", answers)
	}

	// Static network + PXE is rejected with a carrier pointer.
	static := base
	static.Netboot = &render.NetbootInputs{NFSRootURL: "10.0.0.1:/export/netboot/toku/iso"}
	static.Network = []render.NetworkEntry{{
		Match:     &render.NetMatch{MAC: "aa:bb:cc:dd:ee:02"},
		Addresses: []string{"172.16.1.12/24"},
	}}
	if _, _, err := d.RenderAnswers(static, render.MachineView{}); err == nil {
		t.Error("static-net PXE must be rejected")
	}
}

// The netboot carrier declarations: casper from the ISO + NFS tree source.
func TestNetbootDeclarationsUbuntu(t *testing.T) {
	d := New("ubuntu22")
	if c, p := render.NetbootInstallOf(d); c != render.NetbootCarrierISO || p != render.NetbootPoolNFS {
		t.Errorf("carrier/pool = %q/%q, want iso/nfs_tree", c, p)
	}
	if d.PXESupport() != render.SupportFull {
		t.Errorf("ubuntu22 PXE support must be full")
	}
}

// PXE without an NFS media base is rejected — casper's nfsroot would be
// empty and the live root could never mount (qemu verification finding).
func TestRenderNetbootRejectsEmptyNFSRoot(t *testing.T) {
	in := baseNetbootInputs()
	in.Netboot = &render.NetbootInputs{PoolURL: "http://10.0.0.2/netboot/files/toku"} // no NFSRootURL
	if _, _, err := New("ubuntu22").RenderAnswers(in, render.MachineView{}); err == nil {
		t.Fatal("empty NFSRootURL must be rejected")
	}
}

func baseNetbootInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken: "toku", MachineID: "mch_u", Hostname: "node-u1",
		ImageSource: "https://mirror.example/u.iso", RootPassword: "pw",
		AnswerBaseURL: "http://10.0.2.2/render/toku",
		CompleteURL:   "http://10.0.2.2/render/toku/complete",
		Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
			{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
			{Mount: "/", FS: "ext4", SizeMB: 8192},
		}}},
	}
}
