package builder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
)

// extractTarFiles pulls named members out of a .tar.gz archive into destDir
// under new flat names (pure Go — the tarball carrier keeps the netboot
// probe free of external tools, same as the xorriso extraction contracts).
// Missing members are an error: the probe trio (kernel/initramfs/modloop)
// is load-bearing, unlike the optional extras in ExtractBootFiles.
func extractTarFiles(ctx context.Context, tarGzPath, destDir string, wanted map[string]string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f, err := os.Open(tarGzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("builder: %s is not gzip: %w", tarGzPath, err)
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
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
		dst, ok := wanted[hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.Create(destDir + string(os.PathSeparator) + dst)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		found[hdr.Name] = true
		delete(wanted, hdr.Name)
		if len(wanted) == 0 {
			return nil
		}
	}
	return fmt.Errorf("builder: %s lacks %v", tarGzPath, wanted)
}
