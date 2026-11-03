package images

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/store"
)

// The scanner reconciles operator-dropped ISOs into the library: unknown
// files register (dir:// provenance), vanished cache files flag missing,
// restored files return to ready. PG-gated via MAMMOTH_TEST_PG_DSN.
func TestDirectoryScanner(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping directory scanner suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	repo := store.NewImageRepo(db)
	events := store.NewEventRepo(db)
	dir := t.TempDir()
	prev := StabilityWindow
	StabilityWindow = 0
	t.Cleanup(func() { StabilityWindow = prev })

	scanner := &DirectoryScanner{Repo: repo, Events: events, Dir: dir, Logger: slog.Default()}

	payload := []byte("pretend this is an iso payload — the sniffer declines garbage and the row still lands")
	if werr := os.WriteFile(filepath.Join(dir, "lab-image.iso"), payload, 0o644); werr != nil {
		t.Fatal(werr)
	}

	count := func() int {
		rows, lerr := repo.List(ctx)
		if lerr != nil {
			t.Fatalf("list: %v", lerr)
		}
		n := 0
		for _, r := range rows {
			if r.SourceURL == "dir://lab-image.iso" {
				n++
			}
		}
		return n
	}
	rowBySource := func() *store.Image {
		rows, lerr := repo.List(ctx)
		if lerr != nil {
			t.Fatalf("list: %v", lerr)
		}
		for i := range rows {
			if rows[i].SourceURL == "dir://lab-image.iso" {
				return &rows[i]
			}
		}
		return nil
	}

	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	row := rowBySource()
	if row == nil {
		t.Fatal("unknown file must register")
	}
	if row.State != "ready" {
		t.Fatalf("state = %q, want ready", row.State)
	}
	if row.SHA256 == "" || len(row.SHA256) != 64 {
		t.Fatalf("sha not populated: %q", row.SHA256)
	}
	if _, err := os.Stat(filepath.Join(dir, row.SHA256+".iso")); err != nil {
		t.Fatalf("canonical hardlink missing: %v", err)
	}
	if row.Name != "lab-image" {
		t.Fatalf("name = %q, want lab-image", row.Name)
	}

	// idempotent: a second pass must not duplicate
	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("second pass duplicated rows: %d", n)
	}

	// file removed → missing
	sha := row.SHA256
	if rerr := os.Remove(filepath.Join(dir, sha+".iso")); rerr != nil {
		t.Fatal(rerr)
	}
	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan 3: %v", err)
	}
	if row = rowBySource(); row.State != "missing" {
		t.Fatalf("state after removal = %q, want missing", row.State)
	}

	// file back → ready
	if werr := os.WriteFile(filepath.Join(dir, sha+".iso"), payload, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan 4: %v", err)
	}
	if row = rowBySource(); row.State != "ready" {
		t.Fatalf("state after restore = %q, want ready", row.State)
	}

	// a renamed duplicate of the same content: no second row (content-
	// addressed), and the pass completes cleanly
	if werr := os.WriteFile(filepath.Join(dir, "lab-image-copy.iso"), payload, 0o644); werr != nil {
		t.Fatal(werr)
	}
	time.Sleep(5 * time.Millisecond)
	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("scan 5: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("duplicate content must not create a row: %d", n)
	}

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM images WHERE source_url = 'dir://lab-image.iso'`)
	})
}
