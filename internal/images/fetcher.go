// Package images fetches registered artifacts into the content-addressed
// cache (docs/09-roadmap.md 下一阶段 6): one download at a time
// (bandwidth-bound anyway), sha256-gated, deduped by digest. The worker is
// a sweep like the webhook dispatcher — rows in fetching state are claimed
// by an in-process set, so a process restart re-claims whatever was
// orphaned. One fetch worker per deployment: like the netboot bind, this is
// not a multi-replica-safe role.
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/store"
)

// Fetcher advances registered images from fetching to ready | failed.
type Fetcher struct {
	Repo   *store.ImageRepo
	Events *store.EventRepo
	// Dir is the content-addressed cache root (MediaDir/images).
	Dir    string
	Logger *slog.Logger
	HTTP   *http.Client
	// CanonicalDistro maps a (distro, version) detection onto the registered
	// driver name before it lands in the catalog's distro column — the spec
	// fills that column verbatim, so it must store "kylinv10", not the
	// detect token "kylin" (render.Registry.CanonicalDistro at assembly).
	// nil keeps the raw detection.
	CanonicalDistro func(distro, version string) string

	inFlight map[string]bool
}

// Interval is the sweep cadence; it is also the polling granularity for a
// submission waiting on its image.
const Interval = 2 * time.Second

// Run blocks until ctx is canceled. Downloads run sequentially: the cache
// is bandwidth-bound, and parallel fetches of multi-gigabyte ISOs only add
// contention (a batch of same-distro registrations shares one file anyway
// once the first digest lands).
func (f *Fetcher) Run(ctx context.Context) error {
	f.inFlight = map[string]bool{}
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f.sweep(ctx)
		}
	}
}

func (f *Fetcher) sweep(ctx context.Context) {
	rows, err := f.Repo.Fetching(ctx)
	if err != nil {
		f.logger().Warn("images sweep: query failed", "err", err.Error())
		return
	}
	for i := range rows {
		id := rows[i].ID
		if f.inFlight[id] {
			continue
		}
		f.inFlight[id] = true
		f.process(ctx, &rows[i])
		delete(f.inFlight, id)
	}
}

func (f *Fetcher) process(ctx context.Context, im *store.Image) {
	log := f.logger().With("image", im.ID)

	// Dedupe by digest: identical bytes already cached → share the file,
	// no download. The first registration pays; every later one is a
	// rename on the row.
	if shared, err := f.Repo.ReadyBySHA(ctx, im.SHA256); err == nil && shared.ID != im.ID {
		// inherit the shared row's identification for blank fields —
		// through the canonical map too: the shared row may predate the
		// mapping (its distro column still reads "kylin"), and a new row
		// inheriting that verbatim would re-plant the drift (the map is
		// idempotent on already-canonical names).
		if im.Distro == "" || im.Version == "" {
			sharedDistro := shared.Distro
			if f.CanonicalDistro != nil {
				sharedDistro = f.CanonicalDistro(shared.Distro, shared.Version)
			}
			if derr := f.Repo.SetDetected(ctx, im.ID, sharedDistro, shared.Version); derr != nil {
				log.Warn("dedupe inherit detection failed", "err", derr.Error())
			}
		}
		// same bytes → same compression facts; carry them over so the dd
		// resolution sees the uncompressed digest without a re-hash pass
		if ferr := f.Repo.SetDetectedFormat(ctx, im.ID, shared.Format, shared.RawSHA256, shared.RawSizeBytes); ferr != nil {
			log.Warn("dedupe inherit format failed", "err", ferr.Error())
		}
		im.Format, im.RawSHA256, im.RawSizeBytes = shared.Format, shared.RawSHA256, shared.RawSizeBytes
		if serr := f.Repo.SetReady(ctx, im.ID, shared.SizeBytes, shared.FilePath); serr != nil {
			log.Warn("dedupe share failed", "err", serr.Error())
			return
		}
		f.emit(ctx, "image.ready", im.ID, map[string]any{
			"sha256": im.SHA256, "size_bytes": shared.SizeBytes, "deduplicated": true,
		})
		log.Info("image ready (shared cache)", "sha256", im.SHA256)
		return
	}

	path, size, err := FetchToFile(ctx, f.http(), im.SourceURL, im.SHA256, f.Dir)
	if err != nil {
		cause := err.Error()
		if uerr := f.Repo.SetFailed(ctx, im.ID, cause); uerr != nil {
			log.Warn("set failed row", "err", uerr.Error())
		}
		f.emit(ctx, "image.failed", im.ID, map[string]any{
			"source_url": im.SourceURL, "sha256": im.SHA256, "error": cause,
		})
		log.Warn("image fetch failed", "err", cause)
		return
	}
	// Auto-detect distro/version for blank fields (the ISO is already on
	// disk — detection is free at this point). The registrant's own values
	// always win; absence of a verdict is fine, the row just stays blank.
	if im.Distro == "" || im.Version == "" {
		if fh, oerr := os.Open(path); oerr == nil {
			distro, version, ok := DetectISO(fh)
			fh.Close()
			if ok && (im.Distro == "" || im.Version == "") {
				if f.CanonicalDistro != nil {
					distro = f.CanonicalDistro(distro, version)
				}
				if derr := f.Repo.SetDetected(ctx, im.ID, distro, version); derr == nil {
					if im.Distro == "" && distro != "" {
						im.Distro = distro
					}
					if im.Version == "" && version != "" {
						im.Version = version
					}
					log.Info("image distro detected", "distro", distro, "version", version)
				}
			}
		}
	}
	// Compression facts land BEFORE ready: state=ready is the gate both the
	// dd spec resolution and the file endpoint honor, and a ready row
	// without its uncompressed digest would resolve into a spec the runtime
	// can never verify (the compressed file's digest ≠ the stream's).
	landFormat(ctx, f.Repo, im, path, log)
	if uerr := f.Repo.SetReady(ctx, im.ID, size, path); uerr != nil {
		log.Warn("set ready row", "err", uerr.Error())
		return
	}
	f.emit(ctx, "image.ready", im.ID, map[string]any{
		"source_url": im.SourceURL, "sha256": im.SHA256, "size_bytes": size,
		"format": im.Format, "raw_sha256": im.RawSHA256, "raw_size_bytes": im.RawSizeBytes,
		"distro": im.Distro, "version": im.Version,
	})
	log.Info("image ready", "bytes", size, "path", path, "distro", im.Distro, "version", im.Version)
}

// landFormat sniffs the cache file's compression and, for gzip/xz, streams
// one decompress pass to record the uncompressed digest + byte span — the
// face the dd runtime verifies (docs/09「镜像直写通路」). Detection is a
// 6-byte read; the pass costs CPU only on compressed artifacts (dd delivery
// formats raw.gz/raw.xz), never on ISO media or uncompressed images. Shared
// by the fetch worker and the directory scanner.
func landFormat(ctx context.Context, repo *store.ImageRepo, im *store.Image, path string, log *slog.Logger) {
	fh, err := os.Open(path)
	if err != nil {
		log.Warn("compression detect open failed", "err", err.Error())
		return
	}
	format := DetectFormat(fh)
	fh.Close()
	if format == "" {
		return
	}
	rawSHA, rawSize := im.SHA256, im.SizeBytes
	if format == FormatRawGZ || format == FormatRawXZ {
		fh, oerr := os.Open(path)
		if oerr != nil {
			log.Warn("compression hash open failed", "err", oerr.Error())
			return
		}
		raw, size, herr := UncompressedDigest(ctx, fh, format)
		fh.Close()
		if herr != nil {
			log.Warn("uncompressed digest failed", "format", format, "err", herr.Error())
			return
		}
		rawSHA, rawSize = raw, size
	}
	if ferr := repo.SetDetectedFormat(ctx, im.ID, format, rawSHA, rawSize); ferr != nil {
		log.Warn("format persist failed", "err", ferr.Error())
		return
	}
	im.Format, im.RawSHA256, im.RawSizeBytes = format, rawSHA, rawSize
	log.Info("image compression facts landed", "format", format, "raw_size_bytes", rawSize)
}

// BackfillFormats lands compression facts on ready rows that predate the
// detection (docs/09「镜像直写通路」库镜像压缩识别, migration 00016): one
// pass at boot — rows already carrying a format skip for an open+6-byte
// read, and rows without a cache file stay the scanner's business. Without
// this a legacy compressed dd image would resolve into a spec the runtime
// can never verify (the compressed file's digest ≠ the write stream's).
func BackfillFormats(ctx context.Context, repo *store.ImageRepo, log *slog.Logger) {
	rows, err := repo.List(ctx)
	if err != nil {
		log.Warn("compression backfill: list failed", "err", err.Error())
		return
	}
	for i := range rows {
		row := &rows[i]
		if row.State != "ready" || row.Format != "" || row.FilePath == "" {
			continue
		}
		if _, err := os.Stat(row.FilePath); err != nil {
			continue
		}
		landFormat(ctx, repo, row, row.FilePath, log)
	}
}

func (f *Fetcher) emit(ctx context.Context, typ, id string, payload map[string]any) {
	if f.Events == nil {
		return
	}
	f.Events.Append(ctx, "image", id, typ, payload)
}

func (f *Fetcher) http() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return http.DefaultClient
}

func (f *Fetcher) logger() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// FetchToFile streams sourceURL into the content-addressed cache, enforcing
// the digest gate before anything under Dir is touched: the body lands in a
// temp file, the running sha256 is compared against wantSHA256, and only a
// matching download is renamed into <sha256>.iso. A digest already present
// under Dir skips the download entirely — the file's name IS its checksum,
// so an existing entry is by construction the same bytes (pre-staged media,
// a re-registration, or a concurrent same-digest fetch that landed first).
func FetchToFile(ctx context.Context, client *http.Client, sourceURL, wantSHA256, dir string) (string, int64, error) {
	digest, err := hex.DecodeString(wantSHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", 0, fmt.Errorf("images: registered sha256 %q is not a sha256 hex digest", wantSHA256)
	}
	if !strings.HasPrefix(sourceURL, "http://") && !strings.HasPrefix(sourceURL, "https://") {
		return "", 0, fmt.Errorf("images: source_url %q must be http(s) — local files belong in spec.source, not the library", sourceURL)
	}

	// Digest dedupe up front: an existing <sha256>.iso is the same bytes by
	// construction — do not re-pull gigabytes from the source for them.
	final := filepath.Join(dir, wantSHA256+".iso")
	if st, err := os.Stat(final); err == nil && st.Mode().IsRegular() {
		return final, st.Size(), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("images: %w", err)
	}
	// The Go default UA ("Go-http-client/1.1") is blanket-403'd by public
	// mirror anti-script rules (aliyun/tuna observed) while plain curl with
	// any named UA passes — identify ourselves honestly instead.
	req.Header.Set("User-Agent", "mammoth-image-fetcher (+https://github.com/3th1nk/mammoth)")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("images: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("images: fetch %s: status %s", sourceURL, resp.Status)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, "fetch-*.part")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
	cerr := tmp.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("images: download %s: %w", sourceURL, err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != wantSHA256 {
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("images: checksum mismatch for %s: registered sha256:%s, fetched sha256:%s — bytes do not match the registration, nothing cached",
			sourceURL, wantSHA256, got)
	}

	// A concurrent same-digest fetch may have landed while we downloaded —
	// keep one copy of the bytes.
	if _, err := os.Stat(final); err == nil {
		_ = os.Remove(tmpName)
		return final, size, nil
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	return final, size, nil
}
