package builder

import (
	"context"
	"fmt"
	"path/filepath"
)

// ProbeNetbootOptions configure the network-boot probe payload.
type ProbeNetbootOptions struct {
	// ISOPath is the alpine carrier ISO (local path or URL, already resolved
	// through EnsureISO by the caller).
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
	StaticCIDR   string
	StaticGateway string
	// ModloopURL, when set, is served as the kernel's modloop= parameter —
	// over the network there is no boot media for the initramfs to find the
	// module loopback image on. Empty keeps the default search behavior.
	ModloopURL string
}

// BuildProbeNetboot assembles the ramdisk probe as a network boot tree: the
// alpine kernel/initramfs/modloop extracted into DestDir with the probe
// overlay (report script + boot-services marker) appended to the initramfs
// as a second cpio segment. The kernel processes concatenated cpio archives,
// so the base initramfs is never repacked.
func BuildProbeNetboot(ctx context.Context, opt ProbeNetbootOptions) (BootTree, error) {
	if opt.ISOPath == "" {
		return BootTree{}, fmt.Errorf("builder: alpine ISO path is required")
	}
	if opt.ReportURL == "" {
		return BootTree{}, fmt.Errorf("builder: report URL is required")
	}

	tree, err := ExtractBootFiles(ctx, opt.XorrisoPath, opt.ISOPath, opt.DestDir)
	if err != nil {
		return BootTree{}, err
	}

	// The overlay script names the kernel flavor in its diagnostics; the
	// flavor also pins the in-ISO modloop name the tree already extracted.
	xorriso := opt.XorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}
	flavor, err := alpineKernel(ctx, xorriso, opt.ISOPath)
	if err != nil {
		return BootTree{}, err
	}
	// The overlay rides the initramfs as an appended cpio segment (the
	// tar.gz apkovl form only applies on boot media).
	script := probeScript(opt.ReportURL, opt.StaticCIDR, opt.StaticGateway, flavor)
	if err := appendCpioArchive(filepath.Join(tree.Dir, tree.Initrd), probeOverlayEntries(script)); err != nil {
		return BootTree{}, fmt.Errorf("builder: overlay append: %w", err)
	}

	// Same module args as the ISO carrier, plus early DHCP and the network
	// modloop. Serial console last so init's output follows it (BMC SOL).
	args := "modules=loop,squashfs,sd-mod,usb-storage console=tty0 console=ttyS0,115200 ip=dhcp"
	if opt.ModloopURL != "" {
		args += " modloop=" + opt.ModloopURL
	}
	tree.KernelArgs = args
	return tree, nil
}
