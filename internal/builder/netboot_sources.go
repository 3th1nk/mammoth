package builder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
)

// ExtractDINetboot pulls the debian-installer boot pair out of the distro's
// official netboot tarball. The tarball is the d-i PXE carrier because the
// ISO's own initrd is the cdrom flavour — it expects a mounted disc and
// cannot fetch installer components over the network (docs/06-install-pipeline.md
// §3.3, PXE 安装源策略).
//
// Member naming varies by release: trixie ships
// debian-installer/amd64/{linux,initrd.gz}; the install.amd/* spellings are
// kept as fallback candidates for older layouts.
func ExtractDINetboot(ctx context.Context, netbootTarGz, destDir string) (BootTree, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return BootTree{}, err
	}
	wanted := map[string][]string{
		"vmlinuz":    {"debian-installer/amd64/linux", "install.amd/vmlinuz"},
		"initrd.img": {"debian-installer/amd64/initrd.gz", "install.amd/initrd.gz"},
	}
	found := map[string]bool{}
	finish := func() error {
		for name, ok := range found {
			if !ok {
				return os.ErrNotExist
			}
			_ = name
		}
		return nil
	}
	if err := func() error {
		f, err := os.Open(netbootTarGz)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			for local, candidates := range wanted {
				if found[local] {
					continue
				}
				for _, c := range candidates {
					if hdr.Name != c && hdr.Name != "/"+c && hdr.Name != "./"+c {
						continue
					}
					out, err := os.Create(filepath.Join(destDir, local))
					if err != nil {
						return err
					}
					if _, err := io.Copy(out, tr); err != nil {
						out.Close()
						return err
					}
					out.Close()
					found[local] = true
					break
				}
			}
		}
		return nil
	}(); err != nil {
		_ = os.RemoveAll(destDir)
		return BootTree{}, err
	}
	if err := finish(); err != nil {
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
