package images

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/ulikunitz/xz"
)

// Compression facts (docs/09「镜像直写通路」): magic-byte sniffing plus the
// uncompressed digest the dd runtime verifies. Pure unit — no PG.

func writeFile(t *testing.T, dir, name string, build func(*os.File) error) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := build(f); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return path
}

func TestDetectFormat(t *testing.T) {
	payload := []byte("mammoth dd test payload")
	dir := t.TempDir()

	open := func(path string) *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	gz := writeFile(t, dir, "disk.img.gz", func(f *os.File) error {
		w := gzip.NewWriter(f)
		_, err := w.Write(payload)
		if err != nil {
			return err
		}
		return w.Close()
	})
	if got := DetectFormat(open(gz)); got != FormatRawGZ {
		t.Fatalf("gzip sniffed as %q", got)
	}

	xzPath := writeFile(t, dir, "disk.img.xz", func(f *os.File) error {
		w, err := xz.NewWriter(f)
		if err != nil {
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		return w.Close()
	})
	if got := DetectFormat(open(xzPath)); got != FormatRawXZ {
		t.Fatalf("xz sniffed as %q", got)
	}

	raw := writeFile(t, dir, "disk.img", func(f *os.File) error {
		_, err := f.Write(payload)
		return err
	})
	if got := DetectFormat(open(raw)); got != FormatRaw {
		t.Fatalf("raw sniffed as %q", got)
	}
}

func TestUncompressedDigest(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64KiB, multi-block
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])
	dir := t.TempDir()

	gz := writeFile(t, dir, "disk.img.gz", func(f *os.File) error {
		w := gzip.NewWriter(f)
		if _, err := w.Write(payload); err != nil {
			return err
		}
		return w.Close()
	})
	f, err := os.Open(gz)
	if err != nil {
		t.Fatalf("open gz: %v", err)
	}
	defer f.Close()
	gotHex, gotSize, err := UncompressedDigest(context.Background(), f, FormatRawGZ)
	if err != nil {
		t.Fatalf("gz digest: %v", err)
	}
	if gotHex != wantHex || gotSize != int64(len(payload)) {
		t.Fatalf("gz digest mismatch: %s (%d bytes)", gotHex, gotSize)
	}

	xzPath := writeFile(t, dir, "disk.img.xz", func(f *os.File) error {
		w, err := xz.NewWriter(f)
		if err != nil {
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		return w.Close()
	})
	xf, err := os.Open(xzPath)
	if err != nil {
		t.Fatalf("open xz: %v", err)
	}
	defer xf.Close()
	gotHex, gotSize, err = UncompressedDigest(context.Background(), xf, FormatRawXZ)
	if err != nil {
		t.Fatalf("xz digest: %v", err)
	}
	if gotHex != wantHex || gotSize != int64(len(payload)) {
		t.Fatalf("xz digest mismatch: %s (%d bytes)", gotHex, gotSize)
	}

	// raw makes no sense here — the caller already holds that digest.
	if _, _, err := UncompressedDigest(context.Background(), xf, FormatRaw); err == nil {
		t.Fatalf("raw digest must be rejected")
	}
}
