package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/assets/pxe"
)

// The Windows PXE carrier (docs/compat/distros.md §windows, "wimboot
// over PXE"): wimboot assembles a WinPE memory environment from the media's
// own boot files, and the per-task augmentation rides INTO boot.wim's Setup
// image as SMALL seed files only — autounattend.xml at the root and
// mammoth/task.json. The WinPE ramdisk (X:) is then self-describing: setup
// finds the answer file locally and the completion-callback chain applies
// unchanged. iPXE delivers the files to wimboot, which hands off to the
// Windows boot manager (docs: ipxe.org/wimboot — wimboot is loaded as an
// iPXE "kernel" and takes every file as an initrd line).
//
// NOT baked into boot.wim: install.wim. An augmented boot.wim carrying it
// crosses 4 GiB and Server 2019's bootmgr ramdisk path fails outright —
// 0xc0000225 "\windows\system32\boot\winload.efi ... missing or contains
// errors" reproduced in qemu (2026-09-20), while the same chain boots a
// stock or small-augmented boot.wim fine. The install source therefore
// rides the network (an SMB share — the WDS shape, pending) and setup is
// pointed at it from the seed files.
//
// Boot files taken verbatim from the media tree (single file set serves
// BIOS and UEFI — wimboot patches the BIOS-shaped BCD for UEFI):
//
//	bootmgr       (media root — BIOS boot manager)
//	bootmgfw.efi  (efi/boot/bootx64.efi — UEFI boot manager)
//	BCD           (efi/microsoft/boot/bcd)
//	boot.sdi      (boot/boot.sdi — ramdisk backing image)
//	boot.wim      (sources/boot.wim, small-augmented per task)
//	wimboot       (the loader itself, from the pinned asset)

// WindowsWimbootOptions configures one per-task boot tree.
type WindowsWimbootOptions struct {
	// ISOPath is the local distribution ISO.
	ISOPath string
	// DestDir is the per-task boot tree (MediaDir/netboot/<token>).
	DestDir string
	// CacheDir is the media repository root for the prepared-tree cache
	// (extraction + SetupComplete injection, ISO-sha keyed). Empty disables
	// the cache (per-task temp, same contract as BuildBootISO).
	CacheDir string
	// Seed carries the render answers; autounattend.xml, mammoth/task.json
	// and mammoth/startnet.cmd are baked into boot.wim here (the
	// SetupComplete pair is consumed by the prepared install.wim injection).
	Seed map[string]string
	// Timeout bounds the wim surgery (default 120m — the tree extraction
	// and the wim rewrites are multi-gigabyte work).
	Timeout time.Duration
}

// wimbootSeedContract names the seed files this carrier bakes into boot.wim.
const (
	wimbootUnattendSeed = "autounattend.xml"
	wimbootTaskSeed     = "mammoth/task.json"
	// startnet replaces the stock wpeinit-only script at the WinPE startup
	// hook — it maps the deployment SMB export and launches setup (the
	// install source does NOT ride the wim, see the package comment).
	wimbootStartnetSeed = "mammoth/startnet.cmd"
	wimbootStartnetDest = "/Windows/System32/startnet.cmd"
)

// BuildWindowsWimboot assembles the per-task Windows PXE boot tree.
func BuildWindowsWimboot(ctx context.Context, opt WindowsWimbootOptions) (BootTree, error) {
	if opt.ISOPath == "" || opt.DestDir == "" {
		return BootTree{}, fmt.Errorf("builder: windows wimboot needs ISOPath and DestDir")
	}
	if opt.Timeout == 0 {
		opt.Timeout = 120 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	// The prepared tree contributes the boot files AND the SetupComplete-
	// injected install.wim (ISO-sha cached — shared with the virtual-media
	// path; the injection is generic either way).
	tree, err := ensureWindowsTree(ctx, BootMediaOptions{
		ISOPath: opt.ISOPath,
		// legacy (no-cache) temp home: next to the boot tree, dies with it
		OutputPath: filepath.Join(opt.DestDir, "prepare"),
		CacheDir:   opt.CacheDir,
	}, opt.Seed)
	if err != nil {
		return BootTree{}, err
	}

	if err := os.MkdirAll(opt.DestDir, 0o755); err != nil {
		return BootTree{}, err
	}
	for _, f := range []struct{ src, dest string }{
		{filepath.Join(tree, "bootmgr"), "bootmgr"},
		{filepath.Join(tree, "efi", "boot", "bootx64.efi"), "bootmgfw.efi"},
		{filepath.Join(tree, "efi", "microsoft", "boot", "bcd"), "BCD"},
		{filepath.Join(tree, "boot", "boot.sdi"), "boot.sdi"},
		{filepath.Join(tree, "sources", "boot.wim"), "boot.wim"},
	} {
		if err := copyFileInto(f.src, filepath.Join(opt.DestDir, f.dest)); err != nil {
			return BootTree{}, fmt.Errorf("builder: windows wimboot media file %s: %w", f.dest, err)
		}
	}
	wb, err := pxe.Files.Open("wimboot")
	if err != nil {
		return BootTree{}, fmt.Errorf("builder: wimboot asset: %w", err)
	}
	defer wb.Close()
	if err := writeFileInto(filepath.Join(opt.DestDir, "wimboot"), wb); err != nil {
		return BootTree{}, fmt.Errorf("builder: wimboot loader: %w", err)
	}

	// Per-task augmentation: the answer file and the task config (KB-scale).
	// install.wim stays OUT — see the package comment (4 GiB bootmgr limit).
	wimPath := filepath.Join(opt.DestDir, "boot.wim")
	idx, err := wimBootIndex(ctx, wimPath)
	if err != nil {
		return BootTree{}, err
	}
	for _, f := range []struct{ seedName, dest string }{
		{wimbootUnattendSeed, "/autounattend.xml"},
		{wimbootTaskSeed, "/" + wimbootTaskSeed},
		{wimbootStartnetSeed, wimbootStartnetDest},
	} {
		content, ok := opt.Seed[f.seedName]
		if !ok || content == "" {
			return BootTree{}, fmt.Errorf("builder: windows wimboot seed %s missing", f.seedName)
		}
		src := filepath.Join(opt.DestDir, ".seed-"+filepath.Base(f.seedName))
		if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
			return BootTree{}, err
		}
		defer os.Remove(src)
		// The stock boot.wim carries its own startnet.cmd — wimlib's add
		// refuses an existing destination, hence the delete-then-add dance
		// (same idiom as the SetupComplete injection).
		if f.seedName == wimbootStartnetSeed {
			del := exec.CommandContext(ctx, "wimlib-imagex", "update", wimPath, idx,
				"--command=delete "+f.dest)
			_ = del.Run()
		}
		out, uerr := exec.CommandContext(ctx, "wimlib-imagex", "update", wimPath, idx,
			"--command=add "+src+" "+f.dest).CombinedOutput()
		if uerr != nil {
			return BootTree{}, fmt.Errorf("wimlib-imagex update (add %s: builder image must ship wimlib): %w: %s",
				f.dest, uerr, tail(out, 400))
		}
	}

	return BootTree{
		Dir:    opt.DestDir,
		Kernel: "wimboot",
		Initrd: "boot.wim",
		Extra: map[string]string{
			"bootmgr":  "bootmgr",
			"bootmgfw": "bootmgfw.efi",
			"bcd":      "BCD",
			"bootsdi":  "boot.sdi",
		},
	}, nil
}

// wimBootIndex resolves the wim image bootmgr boots (the "Boot Index" in
// wimlib's info output; the media contract pins it to the Setup image, 2).
func wimBootIndex(ctx context.Context, wim string) (string, error) {
	out, err := exec.CommandContext(ctx, "wimlib-imagex", "info", wim).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("wimlib-imagex info: %w: %s", err, tail(out, 400))
	}
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(t, "Boot Index:"); ok {
			if idx := strings.TrimSpace(rest); idx != "" {
				return idx, nil
			}
		}
	}
	return "2", nil // media contract default: the Microsoft Windows Setup image
}

// copyFileInto copies src into dest (plain file, 0644 — the sources are the
// extracted media tree, possibly read-only).
func copyFileInto(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFileInto(dest, in)
}

func writeFileInto(dest string, in io.Reader) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
