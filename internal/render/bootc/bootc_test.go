package bootc

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func bootcInputs(source string) render.InstallInputs {
	return render.InstallInputs{
		TaskToken:     "tok1",
		MachineID:     "mach-1",
		ImageSource:   source,
		AnswerBaseURL: "http://engine:8091/render/tok1",
		CompleteURL:   "http://engine:8091/render/tok1/complete",
		AgentLogURL:   "http://engine:8091/render/tok1/agentlog",
		Disks:         []render.ResolvedDisk{{Device: "/dev/nvme0n1", SizeBytes: 4000787030016}},
	}
}

// The plan's executable face: dispatch flag, OCI reference, the target
// device, keys, and the token-scoped URLs — the dd plan's shape with bootc's
// payload.
func TestRenderSH(t *testing.T) {
	d := New("bootc")
	in := bootcInputs("quay.io/centos/centos-bootc:stream9")
	in.SSHPublicKeys = []string{"ssh-ed25519 AAA operator@host"}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	plan := answers[0].Content
	for _, want := range []string{
		"MAMMOTH_BOOTC=1",
		`MAMMOTH_BOOTC_REF='quay.io/centos/centos-bootc:stream9'`,
		`MAMMOTH_BOOTC_TARGET='/dev/nvme0n1'`,
		"MAMMOTH_SSH_KEYS='ssh-ed25519 AAA operator@host'",
		`MAMMOTH_COMPLETE_URL='http://engine:8091/render/tok1/complete'`,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "MAMMOTH_SEED=1") {
		t.Errorf("slice-1 bootc carries no seed:\n%s", plan)
	}
}

// The boot line carries alpine_repo UNCONDITIONALLY on both carriers —
// podman is the pathway's hard prerequisite.
func TestBootParamsCarryPool(t *testing.T) {
	d := New("bootc")
	in := bootcInputs("registry.example/os/bootc@sha256:abc123")
	_, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(boot.KernelArgs, "alpine_repo=") {
		t.Errorf("ISO boot line must carry alpine_repo (podman source): %s", boot.KernelArgs)
	}
	if !strings.Contains(boot.NetbootKernelArgs, "alpine_repo=") {
		t.Errorf("netboot boot line must carry alpine_repo: %s", boot.NetbootKernelArgs)
	}
	if !boot.InstallerAutoReboot {
		t.Error("the runtime reboots itself after to-disk — InstallerAutoReboot must be set")
	}
}

// imageRef: docker:// stripped, bare refs pass, ISO/PATH shapes rejected.
func TestImageRef(t *testing.T) {
	ok := map[string]string{
		"quay.io/centos/centos-bootc:stream9":          "quay.io/centos/centos-bootc:stream9",
		"docker://quay.io/centos/centos-bootc:stream9": "quay.io/centos/centos-bootc:stream9",
		"registry.example/os@sha256:abc":               "registry.example/os@sha256:abc",
	}
	for in, want := range ok {
		got, err := imageRef(in)
		if err != nil || got != want {
			t.Errorf("imageRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"https://mirror.example/os.iso",
		"http://mirror.example/os.iso",
		"file:///srv/os.iso",
		"nfs://host/export/os.iso",
		"/srv/os.iso",
		"",
	} {
		if _, err := imageRef(bad); err == nil {
			t.Errorf("imageRef(%q) must reject non-OCI shapes", bad)
		}
	}
}

// Slice-1 intent boundary: keys ride the native flag; hostname/network/
// password intent is a render-time honest reject — never silently dropped.
func TestSeedIntentRejected(t *testing.T) {
	d := New("bootc")
	cases := map[string]func(*render.InstallInputs){
		"hostname":      func(in *render.InstallInputs) { in.Hostname = "node-1" },
		"root password": func(in *render.InstallInputs) { in.RootPassword = "x" },
		"network": func(in *render.InstallInputs) {
			in.Network = []render.NetworkEntry{{Match: &render.NetMatch{MAC: "aa:bb:cc:dd:ee:01"}, Addresses: []string{"192.168.1.221/24"}}}
		},
	}
	for name, mut := range cases {
		in := bootcInputs("quay.io/centos/centos-bootc:stream9")
		in.SSHPublicKeys = []string{"ssh-ed25519 AAA operator@host"}
		mut(&in)
		if _, _, err := d.RenderAnswers(in, render.MachineView{}); err == nil {
			t.Errorf("%s intent must be a render-time reject (slice-1 boundary)", name)
		}
	}
	// keys-only passes
	in := bootcInputs("quay.io/centos/centos-bootc:stream9")
	in.SSHPublicKeys = []string{"ssh-ed25519 AAA operator@host"}
	if _, _, err := d.RenderAnswers(in, render.MachineView{}); err != nil {
		t.Errorf("keys-only spec must render: %v", err)
	}
}
