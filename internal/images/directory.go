package images

// DirectoryScanner reconciles the media images directory with the library
// (docs/04-install-spec.md §4.1): ISOs an operator copies in out of band
// are registered automatically, and library rows whose cache file was
// removed are flagged instead of surfacing as an opaque fetch failure at
// install time.
//
// Semantics (one scan pass):
//   - canonical files (<sha256>.iso) hash for free — the name IS the
//     digest; other names are hashed once and remembered through the row
//     (source_url records dir://<filename>, so later passes skip hashing).
//   - unknown files are hardlinked to their canonical <sha256>.iso name —
//     FetchToFile's pre-staged rule ("the file's name IS its checksum")
//     then makes any API registration of the same digest instantly ready.
//   - a ready row whose canonical file vanished → state missing; the file
//     returning (or a fresh copy under any name) → back to ready. Rows are
//     never auto-deleted: an install referencing the cache file must fail
//     loudly, not silently.
//   - unstable files (mtime inside the stability window — an scp still in
//     flight) are picked up by a later pass.
//
// Hashing cost is bounded: only files new to the library are hashed; the
// pass itself is a readdir + stat otherwise.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/store"
)

// ScanInterval is the reconciliation cadence. Files touched within
// StabilityWindow of a pass are left for the next one — an in-flight scp
// must not be hashed or registered mid-write.
const (
	ScanInterval = 60 * time.Second
)

// StabilityWindow is the file-age below which a pass skips a file (an scp
// still in flight must not be hashed mid-write). Var: tests pin it to zero.
var StabilityWindow = ScanInterval

const ()

type DirectoryScanner struct {
	Repo   *store.ImageRepo
	Events *store.EventRepo
	// Dir is the media images directory (MediaDir/images) — the same
	// content-addressed cache the fetch worker fills.
	Dir    string
	Logger *slog.Logger
}

func (s *DirectoryScanner) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Run blocks until ctx is canceled, reconciling once per interval.
func (s *DirectoryScanner) Run(ctx context.Context) error {
	ticker := time.NewTicker(ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Scan(ctx); err != nil {
				s.logger().Warn("images directory scan failed", "err", err.Error())
			}
		}
	}
}

// shaForFile resolves a file's digest: canonical names carry it for free,
// anything else is hashed (streaming — ISOs are gigabytes).
func shaForFile(path, name string) (string, error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if len(base) == 64 && isHex(base) {
		return strings.ToLower(base), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) > 0
}

// Scan reconciles one pass. Errors on the directory walk itself; per-file
// problems are logged and skipped (the next pass retries).
func (s *DirectoryScanner) Scan(ctx context.Context) error {
	log := s.logger()

	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return fmt.Errorf("images: read dir: %w", err)
	}
	rows, err := s.Repo.List(ctx)
	if err != nil {
		return fmt.Errorf("images: list rows: %w", err)
	}

	// Reconcile ready/missing rows against the canonical file's existence.
	canonical := func(sha string) string { return filepath.Join(s.Dir, sha+".iso") }
	shaByPath := map[string]string{}
	for _, r := range rows {
		if r.State == "ready" || r.State == "missing" {
			st, err := os.Stat(canonical(r.SHA256))
			exists := err == nil && st.Mode().IsRegular()
			switch {
			case r.State == "ready" && !exists:
				if merr := s.Repo.SetMissing(ctx, r.ID, "cache file removed from the media images directory"); merr != nil {
					log.Warn("mark missing failed", "image", r.ID, "err", merr.Error())
					continue
				}
				s.emit(ctx, "image.missing", r.ID, map[string]any{"sha256": r.SHA256})
				log.Info("image cache file missing", "image", r.ID, "sha256", r.SHA256)
			case r.State == "missing" && exists:
				if rerr := s.Repo.SetReady(ctx, r.ID, st.Size(), canonical(r.SHA256)); rerr != nil {
					log.Warn("restore failed", "image", r.ID, "err", rerr.Error())
					continue
				}
				s.emit(ctx, "image.ready", r.ID, map[string]any{"sha256": r.SHA256, "restored": true})
				log.Info("image cache file restored", "image", r.ID, "sha256", r.SHA256)
			}
		}
		shaByPath[r.SourceURL] = r.SHA256 // dir://<name> provenance → known digest
	}

	// Register unknown files. Fetching rows are the fetch worker's business:
	// their partial or canonical files are skipped here wholesale.
	known := map[string]bool{} // sha → already in the library
	for _, r := range rows {
		known[r.SHA256] = true
	}
	now := time.Now()
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.EqualFold(filepath.Ext(name), ".iso") {
			continue
		}
		path := filepath.Join(s.Dir, name)

		// canonical name → digest for free; skip files whose digest the
		// library already knows (registered, fetching, or dir-imported).
		sha := ""
		if base := strings.TrimSuffix(name, filepath.Ext(name)); len(base) == 64 && isHex(base) {
			sha = strings.ToLower(base)
		} else if prev, ok := shaByPath["dir://"+name]; ok {
			sha = prev // hashed on a previous pass — remembered in the row
		}
		if sha != "" && known[sha] {
			continue
		}

		fi, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) < StabilityWindow {
			continue // an scp still in flight — next pass
		}
		if sha == "" {
			if sha, err = shaForFile(path, name); err != nil {
				log.Warn("images scan: hash failed", "file", name, "err", err.Error())
				continue
			}
		}
		if known[sha] {
			continue // user duplicate of a known digest (e.g. renamed copy)
		}

		if err := s.importFile(ctx, path, name, sha, fi.Size(), log); err != nil {
			log.Warn("images scan: import failed", "file", name, "err", err.Error())
			continue
		}
		known[sha] = true
	}
	return nil
}

// importFile registers one directory file: hardlink it to its canonical
// <sha256>.iso name (FetchToFile's pre-staged rule then makes API
// registrations of the same digest instantly ready), detect the
// distro/version, and set the row ready with dir:// provenance.
func (s *DirectoryScanner) importFile(ctx context.Context, path, name, sha string, size int64, log *slog.Logger) error {
	canonical := filepath.Join(s.Dir, sha+".iso")
	if _, err := os.Stat(canonical); err != nil {
		if lerr := os.Link(path, canonical); lerr != nil {
			return fmt.Errorf("canonical hardlink: %w", lerr)
		}
	}

	im := &store.Image{
		ID:        store.NewID("img"),
		Name:      strings.TrimSuffix(name, filepath.Ext(name)),
		SourceURL: "dir://" + name, // provenance: imported from the media dir
		SHA256:    sha,
		State:     "fetching",
	}
	if err := s.Repo.Create(ctx, im); err != nil {
		return err
	}
	distro, version := "", ""
	if fh, oerr := os.Open(canonical); oerr == nil {
		d, v, ok := DetectISO(fh)
		fh.Close()
		if ok {
			distro, version = d, v
		}
	}
	if distro != "" || version != "" {
		if derr := s.Repo.SetDetected(ctx, im.ID, distro, version); derr != nil {
			log.Warn("detection persist failed", "image", im.ID, "err", derr.Error())
		}
	}
	if rerr := s.Repo.SetReady(ctx, im.ID, size, canonical); rerr != nil {
		return rerr
	}
	s.emit(ctx, "image.ready", im.ID, map[string]any{
		"sha256": sha, "size_bytes": size, "distro": distro, "version": version,
		"source": "directory",
	})
	log.Info("image imported from media directory", "image", im.ID, "file", name,
		"sha256", sha, "distro", distro, "version", version)
	return nil
}

func (s *DirectoryScanner) emit(ctx context.Context, typ, id string, payload map[string]any) {
	if s.Events == nil {
		return
	}
	payload["image_id"] = id
	s.Events.Append(ctx, "image", id, typ, payload)
}
