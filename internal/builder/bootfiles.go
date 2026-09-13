package builder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BootTree is the network-boot payload extracted from a distribution ISO:
// flat file names inside Dir, ready to be served over HTTP under
// /netboot/files/<token>/ (docs/06-install-pipeline.md §3.3). BIOS and UEFI
// clients boot the same kernel/initrd pair — the architecture choice happens
// in DHCP, not here.
type BootTree struct {
	// Dir is the absolute directory holding the files (the per-task boot
	// tree: MediaDir/netboot/<token>).
	Dir string
	// Kernel and Initrd are bare file names inside Dir.
	Kernel string
	Initrd string
	// Extra lists auxiliary files the kernel args reference by absolute URL
	// (e.g. the alpine modloop). They join the HTTP file allowlist.
	Extra map[string]string
}

// ExtractBootFiles pulls the network-boot payload out of a distribution ISO
// into destDir with normalized flat names. It reuses the same xorriso
// osirrox extraction as the ISO assembly (user-space, no mounts, no root)
// and the same layout detection — one source of truth for "where does this
// distro keep its boot files".
func ExtractBootFiles(ctx context.Context, xorrisoPath, isoPath, destDir string) (BootTree, error) {
	layout, installDir, err := DetectLayout(ctx, xorrisoPath, isoPath)
	if err != nil {
		return BootTree{}, err
	}
	xorriso := xorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	// DetectLayout answers casper/debian-di/rhel10; "" means classic RHEL
	// isolinux OR alpine (which DetectLayout does not classify — the probe
	// builder pins it directly). Alpine names its boot files by kernel
	// flavor (vmlinuz-lts, initramfs-lts, modloop-lts): resolve the flavor
	// from the boot tree, which doubles as the layout test.
	if layout == layoutAlpine || alpineKernelProbe(ctx, xorriso, isoPath) {
		flavor, ferr := alpineKernel(ctx, xorriso, isoPath)
		if ferr != nil {
			return BootTree{}, ferr
		}
		layout = layoutAlpine
		installDir = flavor
	}
	paths := bootFilePathsFor(layout, installDir)
	if paths.kernel == "" {
		return BootTree{}, fmt.Errorf("builder: unknown boot layout for %s", isoPath)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return BootTree{}, err
	}
	tree := BootTree{Dir: destDir, Extra: map[string]string{}}
	for _, f := range []struct{ iso, local string }{
		{paths.kernel, "vmlinuz"},
		{paths.initrd, "initrd.img"},
		{paths.extra, "modloop"},
	} {
		if f.iso == "" {
			continue
		}
		dest := filepath.Join(destDir, f.local)
		x := exec.CommandContext(ctx, xorriso, "-osirrox", "on", "-indev", isoPath,
			"-extract", "/"+f.iso, dest)
		out, err := x.CombinedOutput()
		if err != nil {
			if f.local == "modloop" {
				continue // aux payload: probe layouts that lack it still boot
			}
			return BootTree{}, fmt.Errorf("extract %s: %w: %s", f.iso, err, tail(out, 300))
		}
		switch f.local {
		case "vmlinuz":
			tree.Kernel = f.local
		case "initrd.img":
			tree.Initrd = f.local
		default:
			tree.Extra["modloop"] = f.local
		}
	}
	if tree.Kernel == "" || tree.Initrd == "" {
		_ = os.RemoveAll(destDir)
		return BootTree{}, fmt.Errorf("builder: %s did not yield a kernel/initrd pair", isoPath)
	}
	return tree, nil
}

// alpineKernelProbe reports whether the ISO carries an alpine boot tree
// (a /boot/vmlinuz-<flavor>) — the layout test for alpine, which
// DetectLayout does not classify.
func alpineKernelProbe(ctx context.Context, xorriso, iso string) bool {
	out, err := exec.CommandContext(ctx, xorriso, "-indev", iso, "-find", "/boot", "-type", "f").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "/boot/vmlinuz-")
}

// bootFilePaths returns the in-ISO paths of the boot payload per layout
// family. Mirror of the extraction map in BuildBootISO and the config paths
// in bootConfigs — keep the three in sync when a layout moves its files.
func bootFilePathsFor(layout Layout, installDir string) (paths struct {
	kernel, initrd, extra string
}) {
	switch layout {
	case layoutCasper:
		paths.kernel, paths.initrd = "casper/vmlinuz", "casper/initrd"
	case layoutDebianDI:
		dir := installDir
		if dir == "" {
			dir = "install.amd"
		}
		paths.kernel = strings.TrimPrefix(dir+"/vmlinuz", "/")
		paths.initrd = strings.TrimPrefix(dir+"/initrd.gz", "/")
	case layoutAlpine:
		// installDir carries the kernel flavor here.
		paths.kernel = "boot/vmlinuz-" + installDir
		paths.initrd = "boot/initramfs-" + installDir
		paths.extra = "boot/modloop-" + installDir
	case layoutRHEL10:
		paths.kernel, paths.initrd = "images/pxeboot/vmlinuz", "images/pxeboot/initrd.img"
	default:
		paths.kernel, paths.initrd = "images/pxeboot/vmlinuz", "images/pxeboot/initrd.img"
	}
	return paths
}
