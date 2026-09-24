package autoinstall

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

// Declared package repos (docs/04-install-spec.md §5.5): deb822 entries in
// the target's sources.list.d, key fetched to the target keyring, suite
// defaulted from the distro. The late-command lines ride JSON/YAML escaping,
// so the assertions decode the base64 bodies the way the installer's shell
// will.
func TestPackageSource(t *testing.T) {
	d := New("ubuntu24")
	in := render.InstallInputs{
		TaskToken:     "tok9",
		MachineID:     "mch_u",
		Hostname:      "node-u1",
		ImageSource:   "https://mirror.example/ubuntu-24.04.iso",
		RootPassword:  "uRoot-pw",
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
		PackageSource: []render.RepoSpec{
			{Name: "internal", URL: "http://mirrors.int/apt", GPGKey: "http://mirrors.int/key.asc", Suite: "custom-suite"},
			{Name: "local", URL: "http://10.0.0.5/apt"},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{ID: "mch_u"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var userdata string
	for _, a := range answers {
		if a.Name == "user-data" {
			userdata = a.Content
		}
	}
	if userdata == "" {
		t.Fatalf("no user-data in %+v", answers)
	}

	// Key fetch rides the installer env's python3 (curl/wget are not
	// guaranteed there) and lands in the TARGET-side keyring.
	if !strings.Contains(userdata, "urllib.request.urlretrieve(") ||
		!strings.Contains(userdata, "http://mirrors.int/key.asc") ||
		!strings.Contains(userdata, "/target/usr/share/keyrings/mammoth-internal.asc") {
		t.Errorf("python3 key fetch line missing or wrong:\n%s", userdata)
	}

	// Decode every sources-entry write and check the decoded bodies.
	re := regexp.MustCompile(`echo ([A-Za-z0-9+/=]{40,}) [|] base64 -d.{0,10}/etc/apt/sources\.list\.d/mammoth-(\w+)\.sources`)
	matches := re.FindAllStringSubmatch(userdata, -1)
	if len(matches) != 2 {
		t.Fatalf("want 2 sources writes, found %d:\n%s", len(matches), userdata)
	}
	decoded := map[string]string{}
	for _, m := range matches {
		raw, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			t.Fatalf("base64 body: %v", err)
		}
		decoded[m[2]] = string(raw)
	}
	want := map[string]string{
		"internal": "Types: deb\nURIs: http://mirrors.int/apt\nSuites: custom-suite\nComponents: main\nSigned-By: /usr/share/keyrings/mammoth-internal.asc\n",
		"local":    "Types: deb\nURIs: http://10.0.0.5/apt\nSuites: noble\nComponents: main\nTrusted: yes\n",
	}
	for name, body := range want {
		if decoded[name] != body {
			t.Errorf("mammoth-%s.sources = %q, want %q", name, decoded[name], body)
		}
	}
}
