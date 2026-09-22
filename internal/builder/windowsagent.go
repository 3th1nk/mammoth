package builder

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/3th1nk/mammoth/assets/win-apply"
)

// WindowsAgentOverlay builds the agent overlay for the windows apply-image
// pathway (boot.installer=agent): the SAME agent runtime script as the
// alpine pilot (its windows-apply branch consumes the windows plan lines)
// plus the pinned wimlib/mkntfs toolchain the extended ISO's /apks pool
// does not carry (assets/win-apply — provenance and refresh procedure
// there). The overlay lands the tools under /usr/local/mammoth-win/; the
// runtime sets LD_LIBRARY_PATH to its lib/ and calls the binaries by
// absolute path.
func WindowsAgentOverlay() (name string, data []byte, err error) {
	entries := agentOverlayEntries(agentScript())
	var tools []cpioEntry
	err = fs.WalkDir(winapply.Tools, "tools", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		rel := strings.TrimPrefix(p, "tools/")
		body, rerr := winapply.Tools.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		mode := int64(0o100644)
		if strings.HasSuffix(p, "/bin/wimlib-imagex") || strings.HasSuffix(p, "/sbin/mkntfs") {
			mode = 0o100755
		}
		tools = append(tools, cpioEntry{
			Name: "usr/local/mammoth-win/" + rel, Mode: mode, Body: body,
		})
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	entries = append(entries, tools...)
	// directory entries for the tool tree (cpio needs parents to exist)
	dirs := []cpioEntry{
		{Name: "usr/local/", Mode: 0o040755},
		{Name: "usr/local/mammoth-win/", Mode: 0o040755},
		{Name: "usr/local/mammoth-win/usr/", Mode: 0o040755},
		{Name: "usr/local/mammoth-win/usr/bin/", Mode: 0o040755},
		{Name: "usr/local/mammoth-win/usr/sbin/", Mode: 0o040755},
		{Name: "usr/local/mammoth-win/usr/lib/", Mode: 0o040755},
	}
	entries = append(dirs, entries...)
	data, err = apkovlArchive(entries)
	if err != nil {
		return "", nil, err
	}
	return AgentOverlayName, data, nil
}

// WindowsTreeOptions configure EnsureWindowsTree — the prepared,
// SetupComplete-injected windows tree (shared cache, pool-store/<sha>).
type WindowsTreeOptions struct {
	// ISOPath is the distro ISO, resolved through EnsureISO by the caller.
	ISOPath string
	// CacheDir is the media repo root for the prepared-tree cache; empty
	// disables caching (build into a transient .winbuild dir).
	CacheDir string
	// Seed carries the generic SetupComplete pair (render/windows
	// SetupCompleteSeedPair) the prepared install.wim gets injected with.
	// Required on a cache miss: EVERY pathway applies this wim and its
	// first-boot callback chain lives in the pair. The agent apply
	// pathway's answer set does not carry the pair itself — passing nil
	// there turned every cache bust into INSTALL_MEDIA_BUILD_FAILED
	// (v4 rebuild, 2026-09-22).
	Seed map[string]string
}

// EnsureWindowsTree materializes the prepared windows tree — the UDF
// extract of the media plus the generic SetupComplete pair injected into
// install.wim (wimlib) — and returns its directory. The wimboot carrier
// consumes it (sources\setup.exe over SMB) and the agent apply-image path
// consumes it (sources/install.wim over HTTP + the ESP boot files); the
// sha-addressed cache is shared between both.
func EnsureWindowsTree(ctx context.Context, opt WindowsTreeOptions) (string, error) {
	return ensureWindowsTree(ctx, BootMediaOptions{ISOPath: opt.ISOPath, CacheDir: opt.CacheDir}, opt.Seed)
}

// WindowsImageIndex resolves the install.wim image index whose /IMAGE/NAME
// matches imageName (SERVERSTANDEARDCORE for the current SKU contract —
// SKU order inside the media is NOT stable, zh-CN 2019 ships Core at
// index 1). The agent apply-image plan carries the resolved index so the
// machine never has to probe SKUs.
func WindowsImageIndex(installWimPath, imageName string) (int, error) {
	info, err := exec.CommandContext(context.Background(), "wimlib-imagex", "info", installWimPath).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("wimlib-imagex info: %w: %s", err, tail(info, 400))
	}
	idx := 0
	for _, line := range strings.Split(string(info), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Index:") {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, "Index:")))
			idx = n
		} else if strings.HasPrefix(t, "Name:") && idx > 0 &&
			strings.Contains(strings.ToUpper(t), strings.ToUpper(imageName)) {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("windows: %q image not found in install.wim (SKU contract mismatch)", imageName)
}


// WindowsSpecializeStrip extracts the sysprep Specialize.xml action file
// from the prepared install.wim image and strips the SpBcd imaging blocks
// (Microsoft-Windows-Sysprep-SpBcd): specialize's online BCD module
// re-opens the BCD store file and FAILED on real hardware with our
// pre-baked store (2288H 9/22 — Status 0xC0000098, "cannot configure
// Windows to run on this hardware" dialog). The module only refreshes
// resume/recovery BCD entries — skipping it is safe for servers, and the
// BCD itself (which bootmgr and the OS already boot from) is untouched.
// The stripped file rides to the machine as an answer file and is injected
// back into the wim before apply (wimlib delete-then-add).
func WindowsSpecializeStrip(installWimPath string, imageIndex int) (string, error) {
	tmp, err := os.MkdirTemp("", "mammoth-spec-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	out, err := exec.CommandContext(context.Background(), "wimlib-imagex", "extract",
		installWimPath, fmt.Sprint(imageIndex),
		"\\Windows\\System32\\Sysprep\\ActionFiles\\Specialize.xml",
		"--dest-dir="+tmp, "--no-acls").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("wimlib extract Specialize.xml: %w: %s", err, tail(out, 300))
	}
	// wimlib extract --dest-dir flattens to the file's base name
	raw, err := os.ReadFile(filepath.Join(tmp, "Specialize.xml"))
	if err != nil {
		return "", err
	}
	stripped := stripSpBcdActions(string(raw))
	if stripped == string(raw) {
		return "", fmt.Errorf("windows: SpBcd block not found in Specialize.xml (image layout drift)")
	}
	return stripped, nil
}

// stripSpBcdActions removes every <imaging> block that references the
// Microsoft-Windows-Sysprep-SpBcd assembly.
func stripSpBcdActions(xml string) string {
	const marker = `name="Microsoft-Windows-Sysprep-SpBcd"`
	var b strings.Builder
	rest := xml
	for {
		i := strings.Index(rest, "<imaging")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.Index(rest[i:], "</imaging>")
		if j < 0 {
			b.WriteString(rest)
			break
		}
		j += i + len("</imaging>")
		if !strings.Contains(rest[i:j], marker) {
			b.WriteString(rest[:j])
		}
		rest = rest[j:]
	}
	return b.String()
}
