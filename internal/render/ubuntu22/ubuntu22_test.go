package ubuntu22

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
		t.Fatal("user-data missing")
	}
	return meta, ud
}

// The autoinstall dialect golden test: seed structure, netplan bond by MAC
// (the batch-stable selector), curtin storage for the wipe path, and the
// completion callback (docs/06-install-pipeline.md §5 matrix: partial).
func TestRenderAutoinstallWipeAndBond(t *testing.T) {
	d := New()
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

	// access: per-task root password via chpasswd, ssh keys via autoinstall.ssh
	if _, ok := ud["chpasswd"]; !ok {
		t.Errorf("root password (chpasswd) missing")
	}
	ssh := auto["ssh"].(map[string]any)
	keys := ssh["authorized-keys"].([]any)
	if len(keys) != 1 || keys[0] != "ssh-ed25519 AAA u@m" {
		t.Errorf("ssh keys wrong: %v", keys)
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
		`"fstype":"fat32"`, `"path":"/boot/efi"`, `"path":"/"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("curtin storage missing %q", want)
		}
	}
	if strings.Contains(joined, "sda") {
		t.Errorf("no sda in this spec, storage leaked: %s", joined)
	}

	// completion callback + user script + hostname
	late := strings.Join(toStrSlice(auto["late-commands"]), "\n")
	if !strings.Contains(late, "https://m/render/tok9/complete") {
		t.Errorf("completion callback missing")
	}
	if !strings.Contains(late, "echo u-done") {
		t.Errorf("user post script missing")
	}
	if !strings.Contains(late, "node-u1") {
		t.Errorf("hostname missing")
	}

	if boot.KernelArgs != "autoinstall ip=dhcp ds=nocloud-net;s=https://m/render/tok9/" {
		t.Errorf("boot params wrong: %q", boot.KernelArgs)
	}
}

// keep: disk on a partial distro: the disk is absent from the curtin config.
func TestRenderKeepDiskPartial(t *testing.T) {
	d := New()
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
	d := New()
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
