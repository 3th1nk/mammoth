package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func baseInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken:     "tok123",
		MachineID:     "m1",
		Hostname:      "node-1",
		ImageSource:   "https://mirror.example/alpine-standard.iso",
		RootPassword:  "s3cret",
		SSHPublicKeys: []string{"ssh-ed25519 AAA key1", "ssh-ed25519 AAA key1", " "},
		BootDrive:     "vda",
		Disks: []render.ResolvedDisk{{
			Device: "vda",
			Partitions: []render.ResolvedPartition{
				{FS: "vfat", Mount: "/boot/efi", SizeMB: 300, Flags: []string{"esp"}},
				{FS: "ext4", Mount: "/", Grow: true},
			},
		}},
		AnswerBaseURL: "http://10.0.0.1:8080/render/tok123",
		CompleteURL:   "http://10.0.0.1:8080/render/tok123/complete",
	}
}

func TestRenderAnswersPlan(t *testing.T) {
	d := New("alpine")
	files, boot, err := d.RenderAnswers(baseInputs(), render.MachineView{})
	if err != nil {
		t.Fatalf("RenderAnswers: %v", err)
	}
	byName := map[string]string{}
	for _, f := range files {
		byName[f.Name] = f.Content
	}
	sh, ok := byName[answerSH]
	if !ok {
		t.Fatalf("no %s in answers (%v)", answerSH, keys(files))
	}
	jsonPlan, ok := byName[answerJSON]
	if !ok {
		t.Fatalf("no %s in answers", answerJSON)
	}

	// sh plan: the facts the agent sources, one line each
	for _, want := range []string{
		"mammoth_disk 'vda'",
		"mammoth_partition 'vda' '/boot/efi' 'vfat' '300' 'esp'",
		"mammoth_partition 'vda' '/' 'ext4' '-' ''",
		"MAMMOTH_HOSTNAME='node-1'",
		"MAMMOTH_ROOT_PASSWORD='s3cret'",
		"MAMMOTH_BOOT_DRIVE='vda'",
		"MAMMOTH_COMPLETE_URL='http://10.0.0.1:8080/render/tok123/complete'",
		"ssh-ed25519 AAA key1",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("plan.sh missing %q", want)
		}
	}
	if n := strings.Count(sh, "ssh-ed25519 AAA key1"); n != 1 {
		t.Errorf("ssh keys not de-duplicated (%d occurrences)", n)
	}

	// JSON plan: parseable and mirrors the same dataset
	var doc struct {
		Version     int    `json:"version"`
		Hostname    string `json:"hostname"`
		BootDrive   string `json:"boot_drive"`
		CompleteURL string `json:"complete_url"`
		Disks       []struct {
			Device     string `json:"device"`
			Partitions []struct {
				FS    string `json:"fs"`
				Mount string `json:"mount"`
				Grow  bool   `json:"grow"`
			} `json:"partitions"`
		} `json:"disks"`
	}
	if err := json.Unmarshal([]byte(jsonPlan), &doc); err != nil {
		t.Fatalf("agent-plan.json not valid JSON: %v\n%s", err, jsonPlan)
	}
	if doc.Version != 1 || doc.Hostname != "node-1" || doc.BootDrive != "vda" {
		t.Errorf("json plan fields wrong: %+v", doc)
	}
	if len(doc.Disks) != 1 || len(doc.Disks[0].Partitions) != 2 || !doc.Disks[0].Partitions[1].Grow {
		t.Errorf("json plan partitions wrong: %+v", doc.Disks)
	}

	// boot params: offline ISO args, PXE args carrying the tree file URLs
	if boot.KernelArgs != isoKernelArgs {
		t.Errorf("ISO kernel args = %q", boot.KernelArgs)
	}
	if !boot.InstallerAutoReboot {
		t.Errorf("agent must reboot itself (InstallerAutoReboot=false)")
	}
	nb := boot.NetbootKernelArgs
	for _, want := range []string{
		"ip=dhcp",
		"modloop=http://10.0.0.1:8080/netboot/files/tok123/modloop",
		"alpine_repo=http://10.0.0.1:8080/netboot/files/tok123/apks",
		"apkovl=http://10.0.0.1:8080/netboot/files/tok123/agent.apkovl.tar.gz",
		"mammoth_base=http://10.0.0.1:8080/render/tok123",
	} {
		if !strings.Contains(nb, want) {
			t.Errorf("netboot args missing %q (got %q)", want, nb)
		}
	}
	if boot.AnswerURL != "http://10.0.0.1:8080/render/tok123/agent-plan.sh" {
		t.Errorf("AnswerURL = %q", boot.AnswerURL)
	}
}

func TestRenderAnswersStaticNetwork(t *testing.T) {
	in := baseInputs()
	in.Network = []render.NetworkEntry{{
		Match:       &render.NetMatch{MAC: "AA:BB:CC:DD:EE:01"},
		Addresses:   []string{"198.51.100.10/24"},
		Routes:      []render.NetRoute{{To: "0.0.0.0/0", Via: "198.51.100.1"}},
		Nameservers: []string{"10.0.0.53"},
	}}
	d := New("alpine")
	files, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("RenderAnswers: %v", err)
	}
	sh := files[1].Content
	for _, want := range []string{
		"mammoth_network 'aa:bb:cc:dd:ee:01' '198.51.100.10/24' '198.51.100.1' '10.0.0.53'",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("plan.sh missing %q", want)
		}
	}
}

func TestRenderAnswersScriptsBase64(t *testing.T) {
	in := baseInputs()
	in.Scripts = []render.ScriptEntry{{
		Stage:  "pre_install",
		Inline: "echo hello",
	}}
	d := New("alpine")
	files, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("RenderAnswers: %v", err)
	}
	if !strings.Contains(files[1].Content, "mammoth_script 'pre_install' 'ZWNobyBoZWxsbw==' '-' '-'") {
		t.Errorf("script entry not rendered base64: %s", files[1].Content)
	}
}

func TestRenderValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*render.InstallInputs)
		want   string
	}{
		{"raid", func(in *render.InstallInputs) {
			in.Raid = []render.ResolvedRaid{{Name: "v0", Level: "1", Mode: "software", Members: []string{"vda"}}}
		}, "RAID"},
		{"bond", func(in *render.InstallInputs) {
			in.Network = []render.NetworkEntry{{Bond: &render.NetBond{Mode: "active-backup"}}}
		}, "bond"},
		{"vlan", func(in *render.InstallInputs) {
			in.Network = []render.NetworkEntry{{VLAN: &render.NetVLAN{ID: 10}}}
		}, "vlan"},
		{"preserve", func(in *render.InstallInputs) {
			in.Disks[0].Partitions[1].Preserve = true
		}, "preservation"},
		{"fs", func(in *render.InstallInputs) {
			in.Disks[0].Partitions[1].FS = "xfs"
		}, "xfs"},
		{"no-root", func(in *render.InstallInputs) {
			in.Disks[0].Partitions[1].Mount = "/data"
		}, "/"},
	}
	for _, tc := range cases {
		in := baseInputs()
		tc.mutate(&in)
		_, _, err := New("alpine").RenderAnswers(in, render.MachineView{})
		if err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err.Error(), tc.want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	d := New("alpine")
	if !render.IsAgentInstaller(d) {
		t.Errorf("driver must declare AgentInstaller")
	}
	if c, p := render.NetbootInstallOf(d); c != render.NetbootCarrierAlpineNetboot || p != render.NetbootPoolNone {
		t.Errorf("netboot capability = (%s, %s)", c, p)
	}
	if render.PXESupport(d) != render.SupportFull {
		t.Errorf("PXE support = %s", render.PXESupport(d))
	}
	if d.KeepPartitionSupport() != render.SupportNone {
		t.Errorf("keep support = %s", d.KeepPartitionSupport())
	}
}

func keys(files []render.AnswerFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}
