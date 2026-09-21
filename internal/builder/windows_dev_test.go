package builder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/windows"
)

// Dev harness: the full windows build chain against a REAL Server 2019 ISO —
// DetectLayout(windows) → repack with the rendered answers at the root →
// wimlib SetupComplete injection → output verification. Skipped unless both
// env vars are set:
//
//	MAMMOTH_WIN_DEV_ISO=~/mammoth-qxe/windows/cn_windows_server_2019_x64_dvd_4de40f33.iso \
//	  go test ./internal/builder -run TestDevWindowsBuildChain -v
func TestDevWindowsBuildChain(t *testing.T) {
	iso := os.Getenv("MAMMOTH_WIN_DEV_ISO")
	if iso == "" {
		t.Skip("MAMMOTH_WIN_DEV_ISO not set; skipping windows dev build")
	}
	outDir := os.Getenv("MAMMOTH_WIN_DEV_OUT")
	if outDir == "" {
		outDir = iso + ".build-test"
	}

	layout, _, err := DetectLayout(context.Background(), "", iso)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if layout != Layout("windows") {
		t.Fatalf("layout = %q, want windows", layout)
	}

	answers, boot, err := windows.New("windows2019").RenderAnswers(render.InstallInputs{
		TaskToken: "devtok", MachineID: "mch_dev", Hostname: "node-dev",
		ImageSource: "file://" + iso, RootPassword: "dev-pw-12345",
		AnswerBaseURL: "http://10.0.2.2:8080/render/devtok",
		CompleteURL:   "http://10.0.2.2:8080/render/devtok/complete",
		Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
			{Mount: "/boot/efi", FS: "vfat", SizeMB: 300, Flags: []string{"esp"}},
			{Mount: "/", FS: "ntfs", Grow: true},
		}}},
		Network: []render.NetworkEntry{{
			Match:       &render.NetMatch{MAC: "aa:bb:cc:dd:ee:ff"},
			Addresses:   []string{"203.0.113.211/24"},
			Routes:      []render.NetRoute{{To: "default", Via: "203.0.113.1"}},
			Nameservers: []string{"223.5.5.5"},
		}},
	}, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	seed := map[string]string{}
	for _, a := range answers {
		seed[a.Name] = a.Content
	}
	out, err := BuildBootISO(context.Background(), BootMediaOptions{
		ISOPath: iso, OutputPath: filepath.Join(outDir, "boot-dev.iso"),
		SeedFiles: seed, Timeout: 0,
		CacheDir: os.Getenv("MAMMOTH_WIN_CACHE"),
	}, boot.KernelArgs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The output is an ISO9660/UDF bridge (genisoimage) — verify through the
	// UDF view (7z), which is what Windows Setup reads.
	grep := func(args ...string) string {
		o, e := exec.Command(args[0], args[1:]...).CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %v: %s", args, e, tail(o, 300))
		}
		return string(o)
	}
	listing := grep("7z", "l", "-tUDF", out)
	if !strings.Contains(listing, "autounattend.xml") {
		t.Errorf("autounattend.xml missing at ISO root (UDF view): %s", tail([]byte(listing), 300))
	}
	// Extract install.wim and verify the injection landed in the image the
	// unattend installs — selecting it BY NAME proves the SKU contract too
	// (wimlib info --xml carries no file tree, so only a real extract proves
	// the file exists; 7z extraction preserves the sources/ prefix).
	extract := filepath.Join(outDir, "wimcheck")
	os.MkdirAll(extract, 0o755)
	grep("7z", "x", "-tUDF", "-y", "-o"+extract, out, "sources/install.wim")
	wim := filepath.Join(extract, "sources", "install.wim")
	info := grep("wimlib-imagex", "info", wim)
	if !strings.Contains(info, "SERVERSTANDARDCORE") {
		t.Errorf("SERVERSTANDARDCORE image missing from install.wim — /IMAGE/NAME contract mismatch")
	}
	grep("wimlib-imagex", "extract", wim, "Windows Server 2019 SERVERSTANDARDCORE",
		"/Windows/Setup/Scripts/SetupComplete.cmd", "--dest-dir="+filepath.Join(extract, "sc"), "--no-acls")
	sc, err2 := os.ReadFile(filepath.Join(extract, "sc", "SetupComplete.cmd"))
	if err2 != nil {
		t.Fatalf("SetupComplete.cmd not extractable from the installed image: %v", err2)
	}
	if !strings.Contains(string(sc), "mammoth-complete.ps1") {
		t.Errorf("SetupComplete.cmd must launch the generic ps1 (per-task URL belongs to task.json):\n%s", sc)
	}
	// The per-task callback contract rides task.json on the output ISO.
	grep("7z", "x", "-tUDF", "-y", "-o"+extract, out, "mammoth/task.json")
	tj, terr := os.ReadFile(filepath.Join(extract, "mammoth", "task.json"))
	if terr != nil {
		t.Fatalf("task.json not extractable from the output ISO: %v", terr)
	}
	if !strings.Contains(string(tj), "render/devtok/complete") {
		t.Errorf("task.json lost the completion callback:\n%s", tj)
	}
	t.Logf("windows build chain OK: %s", out)
}

// Dev harness: the real-ISO path of DetectWindowsMediaLanguage — the 7z
// single-file extraction layer (the cache and tree layers are covered by
// TestDetectWindowsMediaLanguageLayers). Skipped unless:
//
//	MAMMOTH_WIN_DEV_ISO=~/mammoth-qxe/windows/cn_windows_server_2019_x64_dvd_4de40f33.iso \
//	  go test ./internal/builder -run TestDevWindowsMediaLanguage -v
func TestDevWindowsMediaLanguage(t *testing.T) {
	iso := os.Getenv("MAMMOTH_WIN_DEV_ISO")
	if iso == "" {
		t.Skip("MAMMOTH_WIN_DEV_ISO not set; skipping windows media language dev probe")
	}
	lang, err := DetectWindowsMediaLanguage(context.Background(), iso, "devsha", t.TempDir())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	t.Logf("media language: %s", lang)
	if lang != "zh-cn" {
		t.Errorf("detect = %q, want zh-cn (the cn Server 2019 media)", lang)
	}
}
