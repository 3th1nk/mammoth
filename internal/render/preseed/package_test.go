package preseed

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
)

// Declared package repos (docs/04-install-spec.md §5.5): one-line sources
// in the target's sources.list.d, key fetched by the installer's busybox
// wget to the target keyring, suite defaulted from the distro.
func TestPackageSource(t *testing.T) {
	d := New("debian13")
	in := wipeInputs()
	in.PackageSource = []render.RepoSpec{
		{Name: "internal", URL: "http://mirrors.int/deb", GPGKey: "http://mirrors.int/key.asc", Suite: "custom-suite"},
		{Name: "local", URL: "http://10.0.0.5/deb"},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{
		ID: "mch_d", Hardware: &bmc.HardwareView{Disks: []bmc.DiskView{
			{Name: "sda", SizeBytes: 960197124096},
		}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, _, post := fetchAnswers(t, answers)

	for _, want := range []string{
		// signed repo: signed-by points at the fetched key
		`echo 'deb [arch=amd64 signed-by=/usr/share/keyrings/mammoth-internal.asc] http://mirrors.int/deb custom-suite main' > /target/etc/apt/sources.list.d/mammoth-internal.list`,
		"wget -q -T 10 -O /target/usr/share/keyrings/mammoth-internal.asc 'http://mirrors.int/key.asc'",
		// unsigned declared repo renders trusted — the operator's call
		`echo 'deb [arch=amd64 trusted=yes] http://10.0.0.5/deb trixie main' > /target/etc/apt/sources.list.d/mammoth-local.list`,
	} {
		if !strings.Contains(post, want) {
			t.Errorf("post-install script missing %q", want)
		}
	}
}
