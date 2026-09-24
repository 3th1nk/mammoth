package kickstart

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

// Declared package repos (docs/04-install-spec.md §5.5): `repo` directives
// for the installer + a %post persisting /etc/yum.repos.d entries.
func TestPackageSource(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		TaskToken:     "tok123",
		MachineID:     "mch_x",
		Hostname:      "node-01",
		ImageSource:   "https://mirror.example/rocky9",
		RootPassword:  "s3creT-pw",
		AnswerBaseURL: "https://m/render/tok123",
		CompleteURL:   "https://m/render/tok123/complete",
		BootDrive:     "nvme0n1",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
			}},
		},
		PackageSource: []render.RepoSpec{
			{Name: "internal", URL: "http://mirrors.int/rocky9", GPGKey: "http://mirrors.int/key.asc"},
			{Name: "local", URL: "http://10.0.0.5/pool"},
		},
	}

	answers, _, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content

	// Install-time directives.
	for _, want := range []string{
		"repo --name=mammoth-internal --baseurl=http://mirrors.int/rocky9",
		"repo --name=mammoth-local --baseurl=http://10.0.0.5/pool",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("kickstart missing install-time %q", want)
		}
	}
	// Installed-system persistence: signed repo pins the key, unsigned one
	// renders gpgcheck=0 (the operator's deliberate call, recorded).
	for _, want := range []string{
		"cat > /etc/yum.repos.d/mammoth-internal.repo <<'MAMMOTH_REPO'",
		"[mammoth-internal]\nname=internal\nbaseurl=http://mirrors.int/rocky9\nenabled=1\ngpgcheck=1\ngpgkey=http://mirrors.int/key.asc",
		"cat > /etc/yum.repos.d/mammoth-local.repo <<'MAMMOTH_REPO'",
		"gpgcheck=0",
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("kickstart post-install missing %q", want)
		}
	}
}

func TestPackageSourceEmpty(t *testing.T) {
	d := New("rocky9")
	in := render.InstallInputs{
		TaskToken:     "tok123",
		MachineID:     "mch_x",
		ImageSource:   "https://mirror.example/rocky9",
		BootDrive:     "nvme0n1",
		AnswerBaseURL: "https://m/render/tok123",
		CompleteURL:   "https://m/render/tok123/complete",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/", FS: "xfs", Grow: true},
			}},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(answers[0].Content, "yum.repos.d") {
		t.Errorf("no package_source declared — no repo post-install expected")
	}
}
