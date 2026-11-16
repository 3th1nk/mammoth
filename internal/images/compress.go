// Compression facts for the artifact library (docs/09「镜像直写通路」):
// the library caches the REGISTERED bytes, but the dd runtime verifies the
// UNCOMPRESSED stream that hits the disk (single-pass wget→gunzip→tee→
// sha256sum, plus the read-back over the same bytes). A compressed
// whole-disk image registered in the library therefore needs its
// uncompressed digest + byte span recorded, or spec resolution would pin
// the spec to the compressed file's digest and every write would fail
// verification.
//
// The compression format itself is a magic-byte sniff (gzip 1f 8b, xz
// FD 37 7A 58 5A 00) — deterministic and free, unlike content
// identification (an os-release inside the image's ext4 is unreachable
// without a full decompress + mount, which the engine honestly doesn't
// attempt: dd images stay distro=dd by registration).
package images

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/ulikunitz/xz"
)

// Image format tokens — the same vocabulary as spec.image.format.
const (
	FormatRaw   = "raw"
	FormatRawGZ = "raw.gz"
	FormatRawXZ = "raw.xz"
)

// DetectFormat sniffs a cache file's compression from its leading magic
// bytes. Anything that is not gzip/xz is raw — including ISO 9660 media,
// which the fetcher never routes through the dd decompressors.
func DetectFormat(f *os.File) string {
	head := make([]byte, 6)
	if _, err := f.ReadAt(head, 0); err != nil && err != io.EOF {
		return ""
	}
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return FormatRawGZ
	case len(head) >= 6 && string(head) == "\xfd7zXZ\x00":
		return FormatRawXZ
	default:
		return FormatRaw
	}
}

// UncompressedDigest streams the decompressed image once and returns its
// sha256 hex digest and byte span — the face the dd runtime's write-stream
// hash and read-back verification see. format must be a compressed one
// (raw.gz / raw.xz); raw callers already hold the digest (the registration's).
func UncompressedDigest(ctx context.Context, f *os.File, format string) (string, int64, error) {
	var dec io.Reader
	switch format {
	case FormatRawGZ:
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", 0, err
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			return "", 0, fmt.Errorf("images: gzip open: %w", err)
		}
		defer zr.Close()
		dec = zr
	case FormatRawXZ:
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", 0, err
		}
		xr, err := xz.NewReader(f)
		if err != nil {
			return "", 0, fmt.Errorf("images: xz open: %w", err)
		}
		dec = xr
	default:
		return "", 0, fmt.Errorf("images: uncompressed digest of %q format makes no sense", format)
	}

	hasher := sha256.New()
	size, err := io.Copy(hasher, cancelingReader{ctx: ctx, r: dec})
	if err != nil {
		return "", 0, fmt.Errorf("images: decompress hash: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// cancelingReader surfaces fetch-worker cancellation into a decompression
// pass (a shutdown must not wait out a multi-gigabyte xz).
type cancelingReader struct {
	ctx context.Context
	r   io.Reader
}

func (c cancelingReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
