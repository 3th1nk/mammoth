package builder

import (
	"context"
	"os"
)

// ExtractDINetboot pulls the debian-installer boot pair out of the distro's
// official netboot tarball (netboot.tar.gz: install.amd/vmlinuz +
// install.amd/initrd.gz among the pxelinux scaffolding). The tarball is the
// d-i PXE carrier because the ISO's own initrd is the cdrom flavour — it
// expects a mounted disc and cannot fetch installer components over the
// network (docs/06-install-pipeline.md §3.3, PXE 安装源策略).
func ExtractDINetboot(ctx context.Context, netbootTarGz, destDir string) (BootTree, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return BootTree{}, err
	}
	if err := extractTarFiles(ctx, netbootTarGz, destDir, map[string]string{
		"install.amd/vmlinuz":   "vmlinuz",
		"install.amd/initrd.gz": "initrd.img",
	}); err != nil {
		_ = os.RemoveAll(destDir)
		return BootTree{}, err
	}
	return BootTree{Dir: destDir, Kernel: "vmlinuz", Initrd: "initrd.img",
		Extra: map[string]string{}}, nil
}

// ExtractISOTree unpacks a distro ISO's full tree under destDir — the PXE
// install source for the pool consumers: d-i's HTTP mirror (dists/ + pool/)
// and casper's NFS root (netboot=nfs). The ISO itself stays the read-only
// repository reference; this is per-task scratch like every other boot tree.
func ExtractISOTree(ctx context.Context, xorrisoPath, isoPath, destDir string) error {
	return extractIsoDir(ctx, xorrisoPath, isoPath, "/", destDir)
}
