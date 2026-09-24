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
		// inherit the shared row's identification for blank fields
		if im.Distro == "" || im.Version == "" {
			if derr := f.Repo.SetDetected(ctx, im.ID, shared.Distro, shared.Version); derr != nil {
				log.Warn("dedupe inherit detection failed", "err", derr.Error())
			}
		}
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
	if uerr := f.Repo.SetReady(ctx, im.ID, size, path); uerr != nil {
		log.Warn("set ready row", "err", uerr.Error())
		return
	}
	f.emit(ctx, "image.ready", im.ID, map[string]any{
		"source_url": im.SourceURL, "sha256": im.SHA256, "size_bytes": size,
		"distro": im.Distro, "version": im.Version,
	})
	log.Info("image ready", "bytes", size, "path", path, "distro", im.Distro, "version", im.Version)
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
// (registered twice, downloaded concurrently) wins and the temp copy is
// dropped — the file's name IS its checksum, so an existing entry is by
// construction the same bytes.
func FetchToFile(ctx context.Context, client *http.Client, sourceURL, wantSHA256, dir string) (string, int64, error) {
	digest, err := hex.DecodeString(wantSHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", 0, fmt.Errorf("images: registered sha256 %q is not a sha256 hex digest", wantSHA256)
	}
	if !strings.HasPrefix(sourceURL, "http://") && !strings.HasPrefix(sourceURL, "https://") {
		return "", 0, fmt.Errorf("images: source_url %q must be http(s) — local files belong in spec.source, not the library", sourceURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("images: %w", err)
	}
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

	final := filepath.Join(dir, wantSHA256+".iso")
	if _, err := os.Stat(final); err == nil {
		_ = os.Remove(tmpName) // same digest already cached — keep one copy
		return final, size, nil
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	return final, size, nil
}
