package builder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ProbeNetbootOptions configure the network-boot probe payload.
type ProbeNetbootOptions struct {
	// TarballPath, when set, is the alpine NETBOOT tarball
	// (alpine-netboot-<ver>-x86_64.tar.gz: vmlinuz-lts + initramfs-lts +
	// modloop-lts) and takes precedence over ISOPath. The netboot
	// initramfs carries the full network driver set — real hardware's NIC
	// drivers usually live in the modloop, which needs the network first
	// (the chicken-and-egg that strands the standard-ISO initramfs on a
	// X722 machine room, 2288H finding).
	TarballPath string
	// ApksISOPath is the alpine standard ISO the apks repository is
	// extracted from — the disk-less root installs alpine-base from it or
	// /sbin/init never appears (init drops to its recovery shell right
	// after "Installing packages: 0 MiB in 0 packages"; 2288H finding).
	ApksISOPath string
	// ISOPath is the alpine carrier ISO (local path or URL, already resolved
	// through EnsureISO by the caller); the fallback payload source when
	// TarballPath is empty.
	ISOPath string
	// DestDir is the per-task boot tree (MediaDir/netboot/<token>).
	DestDir string
	// XorrisoPath overrides the xorriso binary (default: PATH lookup).
	XorrisoPath string
	// ReportURL is the machine-face endpoint the probe POSTs its findings to
	// (baked into the overlay — the probe never reads kernel args for it).
	ReportURL string
	// StaticCIDR / StaticGateway are the probe's DHCP fallbacks (same
	// semantics as the virtual-media carrier).
	StaticCIDR    string
	StaticGateway string
	// ModloopURL, when set, is served as the kernel's modloop= parameter —
	// over the network there is no boot media for the initramfs to find the
	// module loopback image on. Empty keeps the default search behavior.
	ModloopURL string
	// ApksURL, when set, becomes the kernel's apks= parameter — the HTTP
	// apk repository the disk-less root installs alpine-base from.
	ApksURL string
}

// BuildProbeNetboot assembles the ramdisk probe as a network boot tree: the
// alpine kernel/initramfs/modloop extracted into DestDir, plus the probe
// overlay packaged as an apkovl (probe.apkovl.tar.gz) the initramfs fetches
// by URL via the apkovl= kernel parameter — alpine's native mechanism for
// boot-configuration overlays, whose files survive the package install.
func BuildProbeNetboot(ctx context.Context, opt ProbeNetbootOptions) (BootTree, error) {
	if opt.TarballPath == "" && opt.ISOPath == "" {
		return BootTree{}, fmt.Errorf("builder: alpine carrier (tarball or ISO) is required")
	}
	if opt.ReportURL == "" {
		return BootTree{}, fmt.Errorf("builder: report URL is required")
	}

	var tree BootTree
	var flavor string
	if opt.TarballPath != "" {
		// Netboot tarball: fixed -lts names, no ISO probing needed. The
		// trio is internally consistent (kernel ↔ modloop versions).
		if err := os.MkdirAll(opt.DestDir, 0o755); err != nil {
			return BootTree{}, err
		}
		if err := extractTarFiles(ctx, opt.TarballPath, opt.DestDir, map[string]string{
			"boot/vmlinuz-lts":   "vmlinuz",
			"boot/initramfs-lts": "initrd.img",
			"boot/modloop-lts":   "modloop",
		}); err != nil {
			return BootTree{}, err
		}
		tree = BootTree{Dir: opt.DestDir, Kernel: "vmlinuz", Initrd: "initrd.img",
			Extra: map[string]string{"modloop": "modloop"}}
		flavor = "lts"
		// The disk-less root needs alpine-base from the ISO's /apks repo —
		// without it /sbin/init never exists (init drops to its recovery
		// shell right after "Installing packages"). Extracted recursively;
		// served as a granted subtree (apks/...).
		if opt.ApksISOPath != "" {
			if err := extractIsoDir(ctx, opt.XorrisoPath, opt.ApksISOPath,
				"/apks", filepath.Join(opt.DestDir, "apks")); err != nil {
				return BootTree{}, fmt.Errorf("builder: apks repo extraction: %w", err)
			}
			tree.Extra["apks"] = "dir:apks"
		}
	} else {
		t, err := ExtractBootFiles(ctx, opt.XorrisoPath, opt.ISOPath, opt.DestDir)
		if err != nil {
			return BootTree{}, err
		}
		tree = t
		xorriso := opt.XorrisoPath
		if xorriso == "" {
			xorriso = "xorriso"
		}
		f, ferr := alpineKernel(ctx, xorriso, opt.ISOPath)
		if ferr != nil {
			return BootTree{}, ferr
		}
		flavor = f
	}
	// The overlay rides as an apkovl fetched by URL (alpine's native
	// mechanism, same content as the virtual-media carrier's on-disk
	// apkovl). Init unpacks it AFTER its DHCP comes up and registers the
	// file list via --overlay-from-stdin, so its /etc files survive the
	// apk --clean-protected that would otherwise delete unowned /etc files
	// during the disk-less package install (2288H console finding).
	ovl, oerr := probeOverlay(opt.ReportURL, opt.StaticCIDR, opt.StaticGateway, flavor)
	if oerr != nil {
		return BootTree{}, oerr
	}
	if werr := os.WriteFile(filepath.Join(tree.Dir, "probe.apkovl.tar.gz"), ovl, 0o644); werr != nil {
		return BootTree{}, werr
	}
	tree.Extra["apkovl"] = "probe.apkovl.tar.gz"

	// Same module args as the ISO carrier, plus early DHCP, the network
	// modloop, the apks repo (the disk-less root's package source), and the
	// apkovl. Serial console last so init's output follows it (BMC SOL).
	args := "modules=loop,squashfs,sd-mod,usb-storage console=tty0 console=ttyS0,115200 ip=dhcp"
	if opt.ModloopURL != "" {
		args += " modloop=" + opt.ModloopURL
	}
	if opt.ApksURL != "" {
		// the disk-less root installs alpine-base from this repo — without
		// it /sbin/init never appears ("OK: 0 MiB in 0 packages" → recovery
		// shell; 2288H console finding). alpine_repo= is the parameter
		// alpine's initramfs-init actually reads (find_boot_repositories).
		args += " alpine_repo=" + opt.ApksURL
	}
	if opt.ModloopURL != "" {
		args += " apkovl=" + strings.TrimSuffix(opt.ModloopURL, "/modloop") + "/probe.apkovl.tar.gz"
	}
	tree.KernelArgs = args
	return tree, nil
}

// extractIsoDir copies a directory subtree out of an ISO with xorriso's
// user-space osirrox (recursive, no mounts, no root) — used for the probe
// netboot's apks repository.
func extractIsoDir(ctx context.Context, xorrisoPath, isoPath, isoDir, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	xorriso := xorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	x := exec.CommandContext(ctx, xorriso, "-osirrox", "on", "-indev", isoPath,
		"-extract", isoDir, destDir)
	out, err := x.CombinedOutput()
	if err != nil {
		return fmt.Errorf("extract %s: %w: %s", isoDir, err, tail(out, 300))
	}
	return nil
}
