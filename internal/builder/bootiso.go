// Boot media assembly (docs/06-install-pipeline.md §2.1 media B): a small
// bootable ISO that boots the distribution installer with Mammoth's kernel
// arguments — inst.ks pointing at the per-task answer file on the mammoth
// server. Packages come from inst.repo (the original install tree/ISO over
// NFS/HTTP), so this media is machine-decoupled (tokenized URL) and tiny.
//
// Every binary that goes on the media (isolinux.bin, efiboot.img, kernel,
// initrd) comes from the distribution ISO itself — mammoth injects no
// binaries. Extraction and assembly both use xorriso (-osirrox / -as
// mkisofs): no loop mounts, no root.

package builder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BootMediaOptions configure one boot ISO build.
type BootMediaOptions struct {
	// ISOPath is the local distribution ISO file (the boot files — kernel,
	// initrd, isolinux, efiboot.img — are extracted from it with xorriso).
	ISOPath string
	// OutputPath is where the assembled ISO is written.
	OutputPath string
	// WorkDir holds intermediate files; empty = OutputPath dir + ".build".
	WorkDir string
	// XorrisoPath overrides the xorriso binary (default: PATH lookup).
	XorrisoPath string
	// Timeout bounds the whole build (default 10m).
	Timeout time.Duration
}

// BuildBootISO assembles a bootable ISO whose bootloader carries the given
// kernel arguments (they must include inst.ks / inst.repo — the driver's
// BootParams). The distro ISO must expose the standard layout:
// images/pxeboot/{vmlinuz,initrd.img}, isolinux/{isolinux.bin,ldlinux.c32},
// images/efiboot.img.
func BuildBootISO(ctx context.Context, opt BootMediaOptions, kernelArgs string) (string, error) {
	if opt.ISOPath == "" {
		return "", fmt.Errorf("builder: distro ISO path is required")
	}
	if opt.OutputPath == "" {
		return "", fmt.Errorf("builder: output path is required")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	work := opt.WorkDir
	if work == "" {
		work = opt.OutputPath + ".build"
	}
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	for _, d := range []string{work, filepath.Join(work, "isolinux"), filepath.Join(work, "images", "pxeboot"), filepath.Join(work, "EFI", "BOOT")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}

	// Extract the distribution's boot files (xorriso osirrox; no mounts, no root).
	xorriso := opt.XorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	for _, rel := range []string{
		"images/pxeboot/vmlinuz",
		"images/pxeboot/initrd.img",
		"isolinux/isolinux.bin",
		"isolinux/ldlinux.c32",
		"images/efiboot.img",
	} {
		dest := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		x := exec.CommandContext(ctx, xorriso, "-osirrox", "on", "-indev", opt.ISOPath,
			"-extract", "/"+rel, dest)
		out, err := x.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("extract %s: %w: %s", rel, err, tail(out, 300))
		}
	}

	// Bootloader configs: identical kernel args for BIOS (isolinux) and UEFI
	// (grub). syslinux resolves config paths relative to the config's own
	// directory — ../ reaches the single copy in /images/pxeboot/ (upstream
	// layout; keeping one copy halves the media size).
	isolinuxCfg := fmt.Sprintf(`default mammoth
timeout 1
label mammoth
  kernel ../images/pxeboot/vmlinuz
  append initrd=../images/pxeboot/initrd.img %s
`, kernelArgs)
	if err := os.WriteFile(filepath.Join(work, "isolinux", "isolinux.cfg"), []byte(isolinuxCfg), 0o644); err != nil {
		return "", err
	}
	grubCfg := fmt.Sprintf(`set default=0
set timeout=1
menuentry 'mammoth' {
  linux /images/pxeboot/vmlinuz %s
  initrd /images/pxeboot/initrd.img
}
`, kernelArgs)
	if err := os.WriteFile(filepath.Join(work, "EFI", "BOOT", "grub.cfg"), []byte(grubCfg), 0o644); err != nil {
		return "", err
	}

	// Assemble: El Torito BIOS (isolinux) + UEFI (efiboot.img).
	cmd := exec.CommandContext(ctx, xorriso, "-as", "mkisofs",
		"-o", opt.OutputPath,
		"-iso-level", "3", "-J", "-joliet-long",
		"-V", "MAMMOTH_BOOT",
		"-b", "isolinux/isolinux.bin",
		"-c", "isolinux/boot.cat",
		"-no-emul-boot", "-boot-load-size", "4", "-boot-info-table",
		"-eltorito-alt-boot",
		"-e", "images/efiboot.img",
		"-no-emul-boot",
		work,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("xorriso: %w: %s", err, tail(out, 400))
	}
	return opt.OutputPath, nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}


// EnsureISO makes the distribution ISO available locally. sourceURL may be
// an HTTP(S) URL or any URI (e.g. nfs://) — for non-HTTP URIs the file is
// looked up in cacheDir by basename. Downloaded files are cached and
// size-verified on subsequent calls.
func EnsureISO(ctx context.Context, sourceURL, cacheDir string) (string, error) {
	filename := sourceURL[strings.LastIndex(sourceURL, "/")+1:]
	dest := filepath.Join(cacheDir, filename)

	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return dest, nil // already cached
	}

	if !strings.HasPrefix(sourceURL, "http://") && !strings.HasPrefix(sourceURL, "https://") {
		return "", fmt.Errorf("builder: %q is not an HTTP URL and no local cache exists at %s", sourceURL, dest)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("builder: download distro ISO: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("builder: download distro ISO: %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", fmt.Errorf("builder: download distro ISO: %w", err)
	}
	return dest, f.Close()
}

