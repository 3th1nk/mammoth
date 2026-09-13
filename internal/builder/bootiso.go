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
	// SeedFiles are answer files baked into the rebuilt image at cloud-init's
	// local seed path (/var/lib/cloud/seed/nocloud/) — casper-layout rebuilds
	// only. The installer reads them from the CD with zero network dependency
	// (the remote-seed path needs casper early networking, a known flake).
	SeedFiles map[string]string
	// Timeout bounds the whole build (default 10m).
	Timeout time.Duration
}

// isoLayout is the boot-layout family a distro ISO belongs to. The layout
// decides both the rebuild strategy (full repack vs selective assembly) and
// which bootloader configs get the mammoth kernel arguments.
type isoLayout string

const (
	layoutCasper   isoLayout = "casper"    // Ubuntu live-server (/casper)
	layoutDebianDI isoLayout = "debian-di" // debian-installer (/install.amd | /install)
	layoutAlpine   isoLayout = "alpine"    // alpine standard (/boot, syslinux.cfg; probe media)
	layoutRHEL10   isoLayout = "rhel10"    // RHEL10-lineage UEFI-only (no isolinux; /images/eltorito.img)
)

// Layout is the exported handle for a boot-layout family (media-space
// budgeting and caller introspection).
type Layout = isoLayout

// DetectLayout classifies a distribution ISO into its boot-layout family.
// Exported for the media-space precheck: full-repack families need ~2x the
// ISO transiently, selective assemblies only the boot files.
func DetectLayout(ctx context.Context, xorrisoPath, isoPath string) (Layout, string, error) {
	if isoHasCasper(ctx, isoPath, xorrisoPath) {
		return layoutCasper, "", nil
	}
	if dir := debianInstallDir(ctx, isoPath, xorrisoPath); dir != "" {
		return layoutDebianDI, dir, nil
	}
	if rhel10Layout(ctx, xorrisoPath, isoPath) {
		return layoutRHEL10, "", nil
	}
	return "", "", nil
}

// FullRepack reports whether the family rebuilds as a full patched image
// (as opposed to a selective boot-media assembly).
func (l Layout) FullRepack() bool {
	switch l {
	case layoutCasper, layoutDebianDI, layoutRHEL10:
		return true
	}
	return false
}

// BuildBootISO assembles a bootable ISO whose bootloader carries the given
// kernel arguments (they must include inst.ks / inst.repo — the driver's
// BootParams). Three layouts are understood: casper (Ubuntu live-server) and
// debian-installer rebuild as a full patched image (both installers read
// packages from the CD itself); the RHEL layout rebuilds as a selective
// boot-media assembly (packages come over the network via inst.repo).
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

	layout, installDir, err := DetectLayout(ctx, opt.XorrisoPath, opt.ISOPath)
	if err != nil {
		return "", err
	}
	if layout.FullRepack() {
		buildSem <- struct{}{}
		defer func() { <-buildSem }()
		return rebuildPatchedISO(ctx, opt, kernelArgs, layout, installDir)
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
	// isolinux.bin / efiboot.img / the kernel pair are the load-bearing set;
	// the .c32 syslinux modules are OPTIONAL — syslinux 4.x (CentOS 7 era)
	// keeps ldlinux inside isolinux.bin and ships no modules at all, and the
	// plain config here uses no menu modules either.
	xorriso := opt.XorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	extracts := map[string]bool{
		"images/pxeboot/vmlinuz":    true,
		"images/pxeboot/initrd.img": true,
		"isolinux/isolinux.bin":     true,
		"isolinux/ldlinux.c32":      false,
		"images/efiboot.img":        true,
	}
	for rel, required := range extracts {
		dest := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		x := exec.CommandContext(ctx, xorriso, "-osirrox", "on", "-indev", opt.ISOPath,
			"-extract", "/"+rel, dest)
		out, err := x.CombinedOutput()
		if err != nil {
			if !required {
				_ = os.Remove(dest)
				continue // older layouts simply don't ship it
			}
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
  linuxefi /images/pxeboot/vmlinuz %s
  initrdefi /images/pxeboot/initrd.img
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
	_ = ensureRemovable(work) // cleanup path — best effort
	_ = os.RemoveAll(work)
	return opt.OutputPath, nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// uriPath extracts the path part of a scheme://host/path URI ("/path");
// empty when the shape doesn't match.
func uriPath(u string) string {
	rest, ok := strings.CutPrefix(u, "nfs://")
	if !ok {
		rest, ok = strings.CutPrefix(u, "cifs://")
	}
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[i:]
	}
	return ""
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
		// Same-host export: mammoth colocated with the NFS server (the
		// common single-node deployment) — the URI's path resolves on this
		// filesystem directly, no copy into the cache needed.
		if p := uriPath(sourceURL); p != "" {
			if fi, serr := os.Stat(p); serr == nil && fi.Mode().IsRegular() {
				return p, nil
			}
		}
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

// rhel10Layout reports the RHEL10-lineage UEFI-only shape: the El Torito
// stub image exists and isolinux is gone (Legacy BIOS boot removed).
func rhel10Layout(ctx context.Context, xorrisoOverride, iso string) bool {
	xorriso := xorrisoOverride
	if xorriso == "" {
		xorriso = "xorriso"
	}
	probe := func(rel string) bool {
		out, err := exec.CommandContext(ctx, xorriso, "-indev", iso, "-find", rel).CombinedOutput()
		return err == nil && strings.Contains(string(out), rel)
	}
	return probe("/images/eltorito.img") && !probe("/isolinux/isolinux.bin")
}

// debianInstallDir returns the d-i kernel directory name ("install.amd" on
// Debian; "install" kept for derivatives/UOS) or "" when the image carries
// no debian-installer layout. Callers MUST probe casper first — Ubuntu
// live-server ships an /install/ directory too.
func debianInstallDir(ctx context.Context, iso, xorrisoOverride string) string {
	xorriso := xorrisoOverride
	if xorriso == "" {
		xorriso = "xorriso"
	}
	for _, dir := range []string{"install.amd", "install"} {
		out, err := exec.CommandContext(ctx, xorriso, "-indev", iso, "-find", "/"+dir, "-type", "d").CombinedOutput()
		if err == nil && strings.Contains(string(out), "/"+dir) {
			return dir
		}
	}
	return ""
}

// rebuildPatchedISO handles full-repack layouts (casper: Ubuntu live-server;
// debian-di: debian-installer netinst): extract everything, patch the boot
// configs with the mammoth entry and bake the seed files at the ISO root,
// then reassemble with the El Torito/MBR/GPT parameters the original image
// reports. Both installers read their packages from the CD, so the whole
// image must survive the repack — a selective boot-media assembly would
// leave them without an install source.
func rebuildPatchedISO(ctx context.Context, opt BootMediaOptions, kernelArgs string, layout isoLayout, installDir string) (string, error) {
	xorriso := opt.XorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	work := opt.WorkDir
	if work == "" {
		work = opt.OutputPath + ".build"
	}
	_ = ensureRemovable(work) // pre-cleanup — best effort
	// A prior failed build leaves a read-only tree behind; restore
	// writability or the cleanup cannot descend into it (RemoveAll fails
	// with "openfdat: permission denied" on darwin).
	_ = exec.CommandContext(ctx, "chmod", "-R", "u+rwX", work).Run()
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
	// debian-cd's report references HOST-side helpers (-isohybrid-mbr
	// /usr/lib/ISOLINUX/isohdpfx.bin) or the SOURCE image via an --interval
	// spec with a relative path — neither exists at assembly time.
	mkisofsArgs = rewriteIsohybridMbr(mkisofsArgs, work, opt.ISOPath)

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
	// auto_chmod_on restores the ISO's recorded modes when it finishes — the
	// tree comes back read-only-ish (dirs often WITHOUT the search bit). The
	// seed/config patching below needs a writable tree, so restore it
	// first: chmod -R handles exotic modes (dirs without the search bit)
	// more reliably than a Go walk (observed on macOS sandboxes); the
	// in-process pass is both fallback and the explicit error surface — a
	// silent failure here would resurface as a misleading "permission
	// denied" on the first WriteFile.
	if err := exec.CommandContext(ctx, "chmod", "-R", "u+rwX", work).Run(); err != nil {
		if ferr := ensureRemovable(work); ferr != nil {
			return "", fmt.Errorf("grant write over extracted tree: %w", ferr)
		}
	}

	// Answer files at the ISO ROOT: the installer mounts the boot medium at
	// /cdrom and reads its seed from there (casper: the nocloud-net
	// file:// seedfrom; d-i: file=/cdrom/preseed.cfg) — fully offline, no
	// installer early networking involved (the initramfs ip= form proved
	// unreliable). Nested names (run/mammoth/*.sh) need their directories.
	seedDir := work
	if len(opt.SeedFiles) > 0 {
		if err := os.MkdirAll(seedDir, 0o755); err != nil {
			return "", err
		}
		for name, content := range opt.SeedFiles {
			dest := filepath.Join(seedDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
				return "", fmt.Errorf("bake seed file %s: %w", name, err)
			}
		}
	}

	if err := patchBootConfigs(work, kernelArgs, layout, installDir); err != nil {
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
	_ = ensureRemovable(work) // cleanup path — best effort
	_ = os.RemoveAll(work)
	return opt.OutputPath, nil
}

// patchBootConfigs overwrites the layout's bootloader configs with a single
// mammoth entry carrying the given kernel arguments.
//
// grub treats ';' as a command separator — the escape applies to grub files
// only (ds=nocloud-net;s=URL survives intact there). isolinux's append line
// has no such special character, and the backslash would leak into the
// kernel command line, so it gets the raw args.
//
// The d-i UEFI chain: the grub binary inside boot/grub/efi.img (the El
// Torito EFI image) carries an embedded config that loads the ISO's
// /boot/grub/grub.cfg — patching that file covers UEFI. A stray
// EFI/boot/grub.cfg is patched too when present, so derivatives that ship
// one don't resurrect a menu without the mammoth args.
func patchBootConfigs(work, kernelArgs string, layout isoLayout, installDir string) error {
	for rel, content := range bootConfigs(kernelArgs, layout, installDir) {
		dest := filepath.Join(work, filepath.FromSlash(rel))
		if layout == layoutDebianDI && !fileExists(filepath.Join(work, filepath.FromSlash(rel))) {
			continue // d-i: only overwrite configs the image actually ships
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// bootConfigs returns {ISO-relative path → config content} for the layout.
// casper: BIOS from /boot/grub/grub.cfg and UEFI from /EFI/boot/grub.cfg,
// kernel/initrd from /casper. debian-di: BIOS from isolinux (both
// isolinux.cfg and the txt.cfg menu the stock config includes), UEFI from
// /boot/grub/grub.cfg, kernel/initrd.gz from the d-i directory.
func bootConfigs(kernelArgs string, layout isoLayout, installDir string) map[string]string {
	grubArgs := strings.ReplaceAll(kernelArgs, ";", "\\;")
	switch layout {
	case layoutCasper:
		grubCfg := fmt.Sprintf(`set default=0
set timeout=1
menuentry 'mammoth' {
	linux /casper/vmlinuz %s
	initrd /casper/initrd
}
`, grubArgs)
		return map[string]string{
			"boot/grub/grub.cfg": grubCfg,
			"EFI/boot/grub.cfg":  grubCfg,
		}
	case layoutRHEL10:
		// RHEL10 UEFI-only: both grub configs (the UEFI one under EFI/BOOT
		// is the live chain; boot/grub2/grub.cfg is the secondary copy) get
		// the single mammoth entry. The embedded grub in efiboot.img finds
		// the config by the volume label it searches — preserved verbatim
		// by the reproduced as_mkisofs parameters. The native `linuxefi/
		// initrdefi` command pair is kept (matches the distro's own menu).
		grub := fmt.Sprintf(`set default="0"
set timeout=5

menuentry 'mammoth' --class fedora --class gnu-linux --class gnu --class os {
	linuxefi /images/pxeboot/vmlinuz %s
	initrdefi /images/pxeboot/initrd.img
}
`, grubArgs)
		return map[string]string{
			"EFI/BOOT/grub.cfg":   grub,
			"boot/grub2/grub.cfg": grub,
		}
	case layoutAlpine:
		// Alpine keeps its BIOS config at /boot/syslinux/syslinux.cfg and
		// its UEFI chain at /boot/grub/grub.cfg (the efi.img grub loads it).
		// installDir carries the kernel flavor ("lts"/"virt").
		isolinux := fmt.Sprintf(`SERIAL 0 115200
TIMEOUT 10
PROMPT 0
DEFAULT probe

LABEL probe
  MENU LABEL mammoth probe
  KERNEL /boot/vmlinuz-%[1]s
  INITRD /boot/initramfs-%[1]s
  APPEND %[2]s
`, installDir, kernelArgs)
		grub := fmt.Sprintf(`set timeout=3
menuentry 'mammoth probe' {
  linux /boot/vmlinuz-%[1]s %[2]s
  initrd /boot/initramfs-%[1]s
}
`, installDir, grubArgs)
		return map[string]string{
			"boot/syslinux/syslinux.cfg": isolinux,
			"boot/grub/grub.cfg":         grub,
		}
	case layoutDebianDI:
		cfgs := debianBootConfigs(kernelArgs, installDir)
		// isolinux resolves the label from isolinux.cfg; txt.cfg is covered
		// defensively (it exists in the stock image and is referenced from
		// some derivative menu chains).
		return map[string]string{
			"isolinux/isolinux.cfg": cfgs.isolinux,
			"isolinux/txt.cfg":      cfgs.isolinux,
			"boot/grub/grub.cfg":    cfgs.grub,
			"EFI/boot/grub.cfg":     cfgs.grub, // conditional — patched only if present
		}
	}
	return nil
}

// debianBootConfigs builds the d-i boot entry pair. d-i arguments need no
// `---` separator (there is no separate target-system argument section) and
// the text installer's kernel is <installDir>/vmlinuz.
type debianCfgs struct{ isolinux, grub string }

func debianBootConfigs(kernelArgs, installDir string) debianCfgs {
	isolinux := fmt.Sprintf(`default mammoth
timeout 1
prompt 0
label mammoth
  kernel /%[1]s/vmlinuz
  append initrd=/%[1]s/initrd.gz %[2]s
`, installDir, kernelArgs)
	grub := fmt.Sprintf(`set default=0
set timeout=1
menuentry 'mammoth' {
	linux /%[1]s/vmlinuz %[2]s
	initrd /%[1]s/initrd.gz
}
`, installDir, strings.ReplaceAll(kernelArgs, ";", "\\;"))
	return debianCfgs{isolinux: isolinux, grub: grub}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rewriteIsohybridMbr repoints -isohybrid-mbr at something that exists at
// assembly time. Two shapes occur in as_mkisofs reports:
//   - a host-side helper path (/usr/lib/ISOLINUX/isohdpfx.bin — absent on the
//     mammoth host): rewritten to the image's own isolinux/isohdpfx.bin, or
//     dropped when the image ships none;
//   - an --interval spec reading the SOURCE image's boot sector (debian-cd);
//     the file is recorded as xorriso received it — often RELATIVE, which
//     breaks at assembly time (different cwd). Repoint it at the absolute
//     source path; drop when unavailable — virtual-media boot needs El
//     Torito, not an isohybrid MBR.
func rewriteIsohybridMbr(args []string, work, srcISO string) []string {
	for i := 1; i < len(args); i++ {
		if args[i-1] != "-isohybrid-mbr" {
			continue
		}
		if v := args[i]; strings.HasPrefix(v, "--interval:") {
			if srcISO != "" {
				if idx := strings.LastIndex(v, ":"); idx >= 0 {
					args[i] = v[:idx+1] + srcISO
					break
				}
			}
			args = append(args[:i-1], args[i+1:]...)
			break
		}
		if local := filepath.Join(work, "isolinux", "isohdpfx.bin"); fileExists(local) {
			args[i] = local
		} else {
			args = append(args[:i-1], args[i+1:]...)
		}
		break
	}
	return args
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

// ensureRemovable grants owner write over a whole tree — ISO extraction
// preserves read-only modes that block patching and cleanup. The first
// chmod error is returned (later ones would just repeat it); best effort
// only where the tree is absent.
func ensureRemovable(root string) error {
	_, statErr := os.Stat(root)
	if statErr != nil {
		return nil // nothing to fix
	}
	var firstErr error
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entry — surface the chmod error instead
		}
		mode := os.FileMode(0o644)
		if info.IsDir() {
			mode = 0o755
		}
		if chErr := os.Chmod(path, mode); chErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("chmod %s: %w", path, chErr)
		}
		return nil
	})
	return firstErr
}
