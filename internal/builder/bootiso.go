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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	// CacheDir is the media repository root for cross-task caches
	// (<CacheDir>/pool-store/<sha>/win — the prepared windows tree). Empty
	// disables caching: the extract happens per task and dies with it.
	CacheDir string
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
	layoutWindows  isoLayout = "windows"   // Windows Server (/sources/install.wim; bootmgr, no config to patch)
)

// Layout is the exported handle for a boot-layout family (media-space
// budgeting and caller introspection).
type Layout = isoLayout

// DetectLayout classifies a distribution ISO into its boot-layout family.
// Exported for the media-space precheck: full-repack families need ~2x the
// ISO transiently, selective assemblies only the boot files.
func DetectLayout(ctx context.Context, xorrisoPath, isoPath string) (Layout, string, error) {
	xorriso := xorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	if isoHasCasper(ctx, isoPath, xorriso) {
		return layoutCasper, "", nil
	}
	if dir := debianInstallDir(ctx, isoPath, xorriso); dir != "" {
		return layoutDebianDI, dir, nil
	}
	if rhel10Layout(ctx, xorriso, isoPath) {
		return layoutRHEL10, "", nil
	}
	if windowsLayout(ctx, xorriso, isoPath) {
		return layoutWindows, "", nil
	}
	// Alpine (standard/virt): a /boot/vmlinuz-<flavor> tree nobody else
	// ships. The flavor doubles as the config installDir (kernel/initrd are
	// named by it). Classification is needed beyond the probe builder now —
	// the agent install path repacks the distro ISO through BuildBootISO.
	if alpineKernelProbe(ctx, xorriso, isoPath) {
		flavor, err := alpineKernel(ctx, xorriso, isoPath)
		if err != nil {
			return "", "", err
		}
		return layoutAlpine, flavor, nil
	}
	return "", "", nil
}

// FullRepack reports whether the family rebuilds as a full patched image
// (as opposed to a selective boot-media assembly).
func (l Layout) FullRepack() bool {
	switch l {
	case layoutCasper, layoutDebianDI, layoutRHEL10, layoutAlpine, layoutWindows:
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
// empty when the shape doesn't match. file:// counts — a file URI IS a
// local path (the e2e/dev harness submits images that way).
func uriPath(u string) string {
	rest, ok := strings.CutPrefix(u, "nfs://")
	if !ok {
		rest, ok = strings.CutPrefix(u, "cifs://")
	}
	if !ok {
		rest, ok = strings.CutPrefix(u, "file://")
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
	defer lockISOFetch(dest)()

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

// isoFetchLocks serializes cache writes per destination: concurrent tasks
// fetching the same source would otherwise interleave appends into one file
// and leave a corrupt cache (resume offsets from two readers disagree).
var isoFetchLocks sync.Map // dest string -> *sync.Mutex

func lockISOFetch(dest string) func() {
	v, _ := isoFetchLocks.LoadOrStore(dest, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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

// sevenZip locates a 7-Zip binary: "7zz" (the newer 7-Zip standalone) or
// "7z" (p7zip). The error surfaces at invocation with the tried names.
func sevenZip() string {
	for _, exe := range []string{"7zz", "7z"} {
		if p, err := exec.LookPath(exe); err == nil {
			return p
		}
	}
	return "7z"
}

// windowsLayout reports the Windows Server media shape: /sources/install.wim
// is the discriminator no other family ships (boot.wim coexists; split
// .swm media is out of scope). The probe MUST force the UDF view (-tUDF):
// a Windows media's ISO9660 tree carries only a lone README — install.wim
// is >4 GiB and UDF-only, and xorriso cannot read UDF directory trees
// (real-media finding: cn_windows_server_2019, 2026-09-19). bootmgr needs
// no mammoth boot arguments — Windows Setup finds autounattend.xml at the
// medium root natively.
func windowsLayout(ctx context.Context, _ string, iso string) bool {
	out, err := exec.CommandContext(ctx, sevenZip(), "l", "-tUDF", iso).CombinedOutput()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "sources/install.wim")
}

// windowsVolumeID reads the volume label straight from the ISO9660 Primary
// Volume Descriptor (LBA 16, bytes 40..71) — present even in UDF-bridge
// media, tool-independent, xorriso-version-proof. Fallback label keeps the
// build alive if the read misfires (the label is cosmetic for unattended
// setup).
func windowsVolumeID(iso string) string {
	const fallback = "MAMMOTH_WIN"
	f, err := os.Open(iso)
	if err != nil {
		return fallback
	}
	defer f.Close()
	pvd := make([]byte, 2048)
	if _, err := f.ReadAt(pvd, 32768); err != nil {
		return fallback
	}
	if string(pvd[1:6]) != "CD001" || pvd[0] != 1 {
		return fallback
	}
	id := strings.TrimRight(string(pvd[40:72]), " ")
	if id == "" {
		return fallback
	}
	return id
}

// windowsAssemblyArgs builds the canonical mkisofs argument set for a
// Windows media: El Torito pairs straight from the extracted tree
// (boot/etfsboot.com BIOS + efi/microsoft/boot/efisys.bin UEFI, both plain
// files on every Server media), UDF is load-bearing (install.wim >4 GiB is
// unreachable through plain ISO9660), and the volume label replays from the
// source PVD so the rebuilt medium keeps the media's own identity.
func windowsAssemblyArgs(outputPath, volid string) []string {
	return []string{
		"-o", outputPath,
		"-V", volid,
		// install.wim exceeds the 4 GiB ISO9660 ceiling: -allow-limited-size
		// opts into the wrapped ISO9660 size while the UDF view carries the
		// true size — the UDF view is what Windows Setup reads.
		"-allow-limited-size",
		"-iso-level", "3", "-J", "-joliet-long", "-D", "-N", "-udf",
		"-b", "boot/etfsboot.com", "-no-emul-boot", "-c", "boot.cat",
		"-boot-load-size", "8",
		"-eltorito-alt-boot", "-e", "efi/microsoft/boot/efisys.bin", "-no-emul-boot",
	}
}

// windowsInjectorVersion guards the prepared-tree cache — bump when the
// injected script pair, its destinations or the SKU contract change, and
// stale trees re-inject on the next build.
const windowsInjectorVersion = "1"

// windows seed-file contract between the driver and the wim injector.
const (
	winSetupCompleteSeed = "mammoth/SetupComplete.cmd"
	winCompletePS1Seed   = "mammoth/mammoth-complete.ps1"
)

// injectWindowsSetupScripts adds the generic SetupComplete pair into the
// install.wim image the unattend installs (%WINDIR%\Setup\Scripts\ —
// Windows runs SetupComplete.cmd as SYSTEM before first logon; the pair is
// generic so the prepared wim is cacheable, per-task variance arrives via
// mammoth/task.json on the medium). The wim edit needs wimlib, not xorriso;
// the builder image ships both. wimlib update takes ONE image (1.13 has no
// ALL selector) and add refuses an existing destination — hence the
// delete-then-add idempotency dance.
func injectWindowsSetupScripts(ctx context.Context, work string, seed map[string]string) error {
	wim := filepath.Join(work, "sources", "install.wim")
	info, err := exec.CommandContext(ctx, "wimlib-imagex", "info", wim).CombinedOutput()
	if err != nil {
		return fmt.Errorf("wimlib-imagex info: %w: %s", err, tail(info, 400))
	}
	// info lists "Index: N" blocks followed by "Name: ..."; find the
	// Standard Core image index (SKU order inside the media is NOT stable —
	// zh-CN 2019 ships Core at index 1).
	idx := ""
	for _, line := range strings.Split(string(info), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Index:") {
			idx = strings.TrimSpace(strings.TrimPrefix(t, "Index:"))
		} else if strings.HasPrefix(t, "Name:") &&
			strings.Contains(strings.ToUpper(t), "SERVERSTANDARDCORE") && idx != "" {
			break
		}
	}
	if idx == "" {
		return fmt.Errorf("windows: SERVERSTANDARDCORE image not found in install.wim (SKU contract mismatch)")
	}
	for _, f := range []struct{ seedName, dest string }{
		{winSetupCompleteSeed, "/Windows/Setup/Scripts/SetupComplete.cmd"},
		{winCompletePS1Seed, "/Windows/Setup/Scripts/mammoth-complete.ps1"},
	} {
		src := filepath.Join(work, filepath.FromSlash(f.seedName))
		if _, serr := os.Stat(src); serr != nil {
			return fmt.Errorf("windows: seed %s missing for wim injection", f.seedName)
		}
		del := exec.CommandContext(ctx, "wimlib-imagex", "update", wim, idx,
			"--command=delete "+f.dest)
		_ = del.Run() // first injection: nothing to delete
		out, uerr := exec.CommandContext(ctx, "wimlib-imagex", "update", wim, idx,
			"--command=add "+src+" "+f.dest).CombinedOutput()
		if uerr != nil {
			return fmt.Errorf("wimlib-imagex update (builder image must ship wimlib): %w: %s", uerr, tail(out, 400))
		}
	}
	return nil
}

// writeWindowsScripts materializes the generic script pair inside the tree
// before wim injection (the injector reads them from the tree; the pair is
// unlink+rewritten so a hardlinked farm never truncates the cache copy).
func writeWindowsScripts(tree string, seed map[string]string) error {
	for _, name := range []string{winSetupCompleteSeed, winCompletePS1Seed} {
		content, ok := seed[name]
		if !ok || content == "" {
			return fmt.Errorf("windows: seed %s missing for wim injection", name)
		}
		dest := filepath.Join(tree, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		_ = os.Remove(dest)
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// extractWindowsTree pulls the full media tree through 7-Zip's forced UDF
// view — osirrox cannot see the UDF-only tree (see windowsLayout).
func extractWindowsTree(ctx context.Context, iso, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, sevenZip(), "x", "-tUDF", "-y",
		"-o"+dst, iso).CombinedOutput()
	if err != nil {
		return fmt.Errorf("extract windows iso (7z -tUDF): %w: %s", err, tail(out, 400))
	}
	if err := exec.CommandContext(ctx, "chmod", "-R", "u+rwX", dst).Run(); err != nil {
		if ferr := ensureRemovable(dst); ferr != nil {
			return fmt.Errorf("grant write over extracted tree: %w", ferr)
		}
	}
	return nil
}

// ensureWindowsTree returns the prepared (extracted + SetupComplete-injected)
// windows tree for the source ISO, cached under
// <CacheDir>/pool-store/<iso sha>/win/tree. The tree and the wim surgery are
// pure ISO-sha derivatives — the injected pair is generic, per-task variance
// rides task.json on the medium — so every task after the first skips the
// extraction and the wim rewrite entirely (pool-store conventions: sha
// addressing, tmp+rename atomicity, 7-day idle sweep inherited). Empty
// CacheDir keeps the legacy per-task temp behavior.
func ensureWindowsTree(ctx context.Context, opt BootMediaOptions, seed map[string]string) (string, error) {
	if opt.CacheDir == "" {
		tree := opt.OutputPath + ".winbuild"
		if err := extractWindowsTree(ctx, opt.ISOPath, tree); err != nil {
			return "", err
		}
		if err := writeWindowsScripts(tree, seed); err != nil {
			return "", err
		}
		if err := injectWindowsSetupScripts(ctx, tree, seed); err != nil {
			return "", err
		}
		return tree, nil
	}
	sha, err := FileSHA256(opt.ISOPath)
	if err != nil {
		return "", fmt.Errorf("windows: iso sha: %w", err)
	}
	dir := filepath.Join(opt.CacheDir, "pool-store", sha, "win")
	tree := filepath.Join(dir, "tree")
	marker := filepath.Join(dir, "injector")
	if fileExists(filepath.Join(tree, "sources", "install.wim")) {
		if b, rerr := os.ReadFile(marker); rerr == nil && strings.TrimSpace(string(b)) == windowsInjectorVersion {
			return tree, nil // cache hit: the whole extract+inject cost is gone
		}
		// stale injection: re-inject into a hardlink copy, atomic rename
		tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%d", os.Getpid()))
		_ = os.RemoveAll(tmp)
		if err := hardlinkTree(tree, tmp); err != nil {
			return "", err
		}
		if err := writeWindowsScripts(tmp, seed); err != nil {
			return "", err
		}
		if err := injectWindowsSetupScripts(ctx, tmp, seed); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
		_ = os.RemoveAll(tree)
		if rerr := os.Rename(tmp, tree); rerr != nil {
			return "", rerr
		}
		_ = os.WriteFile(marker, []byte(windowsInjectorVersion), 0o644)
		return tree, nil
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%d", os.Getpid()))
	_ = os.RemoveAll(tmp)
	if err := extractWindowsTree(ctx, opt.ISOPath, tmp); err != nil {
		return "", err
	}
	if err := writeWindowsScripts(tmp, seed); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := injectWindowsSetupScripts(ctx, tmp, seed); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if fileExists(tree) {
		// lost the race: the winner's content is identical (same sha +
		// injector version) — discard ours
		_ = os.RemoveAll(tmp)
	} else if rerr := os.Rename(tmp, tree); rerr != nil {
		return "", rerr
	}
	_ = os.WriteFile(marker, []byte(windowsInjectorVersion), 0o644)
	return tree, nil
}

// hardlinkTree materializes dst as a hardlink farm of src (instant, zero
// data copy on the same filesystem); unlinkable files (cross-device) fall
// back to plain copies. Callers must os.Remove a linked dest before
// overwriting it — WriteFile on a hardlink truncates the shared inode.
func hardlinkTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		t := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		if lerr := os.Link(p, t); lerr == nil {
			return nil
		}
		in, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(t, in, 0o644)
	})
}

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
	// MBR/GPT layout) — distro-version proof. Windows skips this entirely:
	// its hidden El Torito images make the report fail fatally on older
	// xorriso (1.4.8, real-media finding) and the assembly args are
	// canonical anyway — only the volume label is replayed, read directly
	// from the ISO9660 PVD.
	var mkisofsArgs []string
	volid := ""
	if layout == layoutWindows {
		volid = windowsVolumeID(opt.ISOPath)
	} else {
		report, err := exec.CommandContext(ctx, xorriso, "-indev", opt.ISOPath,
			"-report_el_torito", "as_mkisofs").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("el torito report: %w: %s", err, tail(report, 400))
		}
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
	}

	// Full extract. ISO9660 extraction preserves read-only modes — grant
	// owner write over the tree or the grub.cfg patch below fails.
	// auto_chmod_on is REQUIRED for casper-layout ISOs: osirrox applies the
	// ISO root's recorded mode (0644, no search bit) to the target directory
	// and its own subsequent opens then fail with "openfdat ...: permission
	// denied". It temporarily chmods restored dirs so extraction completes.
	// Full extract. Windows media goes through the cached prepared tree
	// (7-Zip UDF view + wimlib injection, ISO-sha keyed — see
	// ensureWindowsTree); every task after the first just hardlinks it.
	// All other families keep xorriso osirrox.
	var extract []byte
	var err error
	if layout == layoutWindows {
		var tree string
		tree, err = ensureWindowsTree(ctx, opt, opt.SeedFiles)
		if err == nil {
			err = hardlinkTree(tree, work)
		}
		if err != nil {
			return "", fmt.Errorf("windows prepared tree: %w", err)
		}
	} else {
		extract, err = exec.CommandContext(ctx, xorriso, "-osirrox", "on:auto_chmod_on", "-indev", opt.ISOPath,
			"-extract", "/", work).CombinedOutput()
		if err != nil {
			_ = os.WriteFile("/tmp/extract-debug.log", extract, 0o644)
			return "", fmt.Errorf("extract distro iso: %w: %s", err, tail(extract, 600))
		}
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
			// Unlink first: the windows farm hardlinks the cache tree, and
			// WriteFile on a hardlinked dest would truncate the shared inode
			// (corrupting the cache for every other task).
			_ = os.Remove(dest)
			if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
				return "", fmt.Errorf("bake seed file %s: %w", name, err)
			}
		}
	}

	if err := patchBootConfigs(work, kernelArgs, layout, installDir); err != nil {
		return "", err
	}

	var asmExe string
	var asmArgs []string
	if layout == layoutWindows {
		// Windows assembly uses genisoimage, NOT xorriso: libisofs cannot
		// write UDF in any version (-as mkisofs rejects -udf, real-media
		// finding on 1.4.8 AND 1.5.8) and UDF is load-bearing — install.wim
		// (4.3 GiB) exceeds the ISO9660 byte-size ceiling, so a plain
		// ISO9660 medium is unreadable to Windows Setup. genisoimage -udf
		// is the two-decade-standard Windows-media path on Linux; El Torito
		// pairs straight from the extracted tree (boot/etfsboot.com BIOS +
		// efi/microsoft/boot/efisys.bin UEFI), volume label replays from
		// the source PVD.
		asmExe = "genisoimage"
		asmArgs = append(windowsAssemblyArgs(opt.OutputPath, volid), work)
	} else {
		asmExe = xorriso
		asmArgs = append(append([]string{"-as", "mkisofs", "-o", opt.OutputPath}, mkisofsArgs...), work)
	}
	out, err := exec.CommandContext(ctx, asmExe, asmArgs...).CombinedOutput()
	if err != nil {
		_ = os.WriteFile("/tmp/asm-debug.log", append(out, []byte(fmt.Sprintf("\nARGS: %s %v\n", asmExe, asmArgs))...), 0o644)
		return "", fmt.Errorf("%s: %w: %s", asmExe, err, tail(out, 400))
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
	if layout == layoutWindows {
		// bootmgr consumes no mammoth arguments: the unattend is found at
		// the medium root by name, and the El Torito replay below keeps the
		// media's own etfsboot/efisys boot entries intact.
		return nil
	}
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
DEFAULT mammoth

LABEL mammoth
  MENU LABEL mammoth
  KERNEL /boot/vmlinuz-%[1]s
  INITRD /boot/initramfs-%[1]s
  APPEND %[2]s
`, installDir, kernelArgs)
		grub := fmt.Sprintf(`set timeout=3
menuentry 'mammoth' {
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

// FileSHA256 streams a file's SHA-256 — the content address of the shared
// pool store (many tasks reuse one unpacked ISO tree; see provision's pool
// store). A multi-gigabyte ISO hashes in seconds, negligible next to the
// extraction it gates.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("builder: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("builder: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
