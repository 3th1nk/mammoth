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
	"bufio"
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

// buildSem serializes media builds: each build moves gigabytes of
// extract+assemble IO, and N concurrent builds (batch installs) would
// thrash the host. Callers queue; batches pay wall-clock in build count,
// not resource collapse.
var buildSem = make(chan struct{}, 2)

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

	// Ubuntu live-server ISOs carry the installer in /casper with a
	// grub-only boot chain (no isolinux, no images/pxeboot) — they rebuild
	// as a full patched image instead of a selective boot-media assembly.
	if isoHasCasper(ctx, opt.ISOPath, opt.XorrisoPath) {
		buildSem <- struct{}{}
		defer func() { <-buildSem }()
		return rebuildPatchedISO(ctx, opt, kernelArgs)
	}

	work := opt.WorkDir
	if work == "" {
		work = opt.OutputPath + ".build"
	}
	_ = exec.CommandContext(ctx, "chmod", "-R", "u+rwX", work).Run()
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
	// The work tree (gigabytes of extracted ISO) never outlives the build —
	// in server deployments it lives INSIDE the media export.
	ensureRemovable(work)
	_ = os.RemoveAll(work)
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

	isHTTP := strings.HasPrefix(sourceURL, "http://") || strings.HasPrefix(sourceURL, "https://")

	// Size-verify the cache against the source: a truncated download left in
	// the cache must not be mistaken for a complete image (it extracts fine
	// up to the missing tail and then poisons every media build).
	var wantSize int64
	var have int64
	if fi, err := os.Stat(dest); err == nil {
		have = fi.Size()
		if have > 0 && !isHTTP {
			return dest, nil // non-HTTP sources (nfs://) are trusted
		}
	}
	if isHTTP {
		req, herr := http.NewRequestWithContext(ctx, http.MethodHead, sourceURL, nil)
		if herr != nil {
			return "", herr
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr == nil {
			wantSize = resp.ContentLength
			resp.Body.Close()
		}
		if have > 0 && wantSize > 0 && have == wantSize {
			return dest, nil // already cached, complete
		}
	} else if have > 0 {
		return dest, nil
	}

	if !isHTTP {
		return "", fmt.Errorf("builder: %q is not an HTTP URL and no local cache exists at %s", sourceURL, dest)
	}

	// Resume when the cached file is a short prefix of the source.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	if have > 0 && have < wantSize {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("builder: download distro ISO: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
		// resuming
	case resp.StatusCode == http.StatusOK:
		have = 0 // full download
	default:
		return "", fmt.Errorf("builder: download distro ISO: %s", resp.Status)
	}
	flag := os.O_WRONLY | os.O_CREATE
	if have > 0 {
		flag |= os.O_APPEND
	}
	f, err := os.OpenFile(dest, flag, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", fmt.Errorf("builder: download distro ISO: %w", err)
	}
	return dest, f.Close()
}

// isoHasCasper reports whether the ISO carries an Ubuntu casper layout.
func isoHasCasper(ctx context.Context, iso, xorrisoOverride string) bool {
	xorriso := xorrisoOverride
	if xorriso == "" {
		xorriso = "xorriso"
	}
	out, err := exec.CommandContext(ctx, xorriso, "-indev", iso, "-find", "/casper", "-type", "d").CombinedOutput()
	return err == nil && strings.Contains(string(out), "/casper")
}

// rebuildPatchedISO handles casper-layout ISOs (Ubuntu live-server): extract
// everything, replace /boot/grub/grub.cfg with a single mammoth entry (the
// squashfs stays on the CD — the installer boots it directly, no network
// root needed), then reassemble with the El Torito/MBR/GPT parameters the
// original image reports. The volume label is preserved for casper.
func rebuildPatchedISO(ctx context.Context, opt BootMediaOptions, kernelArgs string) (string, error) {
	xorriso := opt.XorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	work := opt.WorkDir
	if work == "" {
		work = opt.OutputPath + ".build"
	}
	ensureRemovable(work)
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}

	// Reproduce parameters straight from the image (label, El Torito entries,
	// MBR/GPT layout) — distro-version proof.
	report, err := exec.CommandContext(ctx, xorriso, "-indev", opt.ISOPath,
		"-report_el_torito", "as_mkisofs").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("el torito report: %w: %s", err, tail(report, 400))
	}
	var mkisofsArgs []string
	scanner := bufio.NewScanner(strings.NewReader(string(report)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "'") && !strings.HasPrefix(line, "\"") {
			continue // report preamble lines
		}
		mkisofsArgs = append(mkisofsArgs, splitQuoted(line)...)
	}

	// Full extract. ISO9660 extraction preserves read-only modes — grant
	// owner write over the tree or the grub.cfg patch below fails.
	// auto_chmod_on is REQUIRED for casper-layout ISOs: osirrox applies the
	// ISO root's recorded mode (0644, no search bit) to the target directory
	// and its own subsequent opens then fail with "openfdat ...: permission
	// denied". It temporarily chmods restored dirs so extraction completes.
	extract, err := exec.CommandContext(ctx, xorriso, "-osirrox", "on:auto_chmod_on", "-indev", opt.ISOPath,
		"-extract", "/", work).CombinedOutput()
	if err != nil {
		_ = os.WriteFile("/tmp/extract-debug.log", extract, 0o644)
		return "", fmt.Errorf("extract distro iso: %w: %s", err, tail(extract, 600))
	}
	ensureRemovable(work)

	// grub treats ';' as a command separator — the cloud-init datasource
	// syntax (ds=nocloud-net;s=URL) must escape it or the kernel command
	// line is truncated at the semicolon and autoinstall never engages.
	grubArgs := strings.ReplaceAll(kernelArgs, ";", "\\;")
	// Ubuntu's hybrid ISO serves UEFI from /EFI/boot/grub.cfg and BIOS from
	// /boot/grub/grub.cfg — both get the mammoth entry.
	grubCfg := fmt.Sprintf(`set default=0
set timeout=1
menuentry 'mammoth' {
	linux /casper/vmlinuz %s
	initrd /casper/initrd
}
`, grubArgs)
	if err := os.WriteFile(filepath.Join(work, "boot", "grub", "grub.cfg"), []byte(grubCfg), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(work, "EFI", "boot", "grub.cfg"), []byte(grubCfg), 0o644); err != nil {
		return "", err
	}

	args := append([]string{"-as", "mkisofs", "-o", opt.OutputPath}, mkisofsArgs...)
	args = append(args, work)
	out, err := exec.CommandContext(ctx, xorriso, args...).CombinedOutput()
	if err != nil {
		_ = os.WriteFile("/tmp/asm-debug.log", append(out, []byte(fmt.Sprintf("\nARGS: %v\n", args))...), 0o644)
		return "", fmt.Errorf("xorriso: %w: %s", err, tail(out, 400))
	}
	// The work tree (gigabytes of extracted ISO) never outlives the build —
	// in server deployments it lives INSIDE the media export.
	ensureRemovable(work)
	_ = os.RemoveAll(work)
	return opt.OutputPath, nil
}

// splitQuoted tokenizes one shell-quoted report line (xorriso uses single
// quotes around values that may contain spaces).
func splitQuoted(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '\'':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ensureRemovable grants owner write over a whole tree, best effort — ISO
// extraction preserves read-only modes that block patching and cleanup.
func ensureRemovable(root string) {
	_, statErr := os.Stat(root)
	if statErr != nil {
		return // nothing to fix
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best effort
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0o755)
		} else {
			_ = os.Chmod(path, 0o644)
		}
		return nil
	})
}
