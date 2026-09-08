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

	// Bootloader configs: identical kernel args for BIOS (isolinux) and UEFI (grub).
	isolinuxCfg := fmt.Sprintf(`default mammoth
timeout 1
label mammoth
  kernel /vmlinuz
  append initrd=/initrd.img %s
`, kernelArgs)
	if err := os.WriteFile(filepath.Join(work, "isolinux", "isolinux.cfg"), []byte(isolinuxCfg), 0o644); err != nil {
		return "", err
	}
	grubCfg := fmt.Sprintf(`set timeout=1
menuentry 'mammoth' {
  linux /vmlinuz %s
  initrd /initrd.img
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

// EnsureISO makes the distribution ISO available locally: if sourceURL is an
// HTTP(S) URL it is downloaded into cacheDir (keyed by filename, skipped when
// already complete); local paths are returned as-is.
func EnsureISO(ctx context.Context, sourceURL, cacheDir string) (string, error) {
	if !strings.Contains(sourceURL, "://") {
		return sourceURL, nil // already a local path
	}
	filename := sourceURL[strings.LastIndex(sourceURL, "/")+1:]
	dest := filepath.Join(cacheDir, filename)

	if fi, err := os.Stat(dest); err == nil {
		// Resume/verify by size against the server.
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, sourceURL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.ContentLength > 0 && fi.Size() == resp.ContentLength {
				return dest, nil // complete
			}
		}
		// incomplete: resume from current size
		return dest, resumeDownload(ctx, sourceURL, dest, fi.Size())
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

func resumeDownload(ctx context.Context, sourceURL, dest string, offset int64) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("builder: server does not support resume (status %d)", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
