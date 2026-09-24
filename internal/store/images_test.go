package store

import (
	"context"
	"os"
	"testing"
)

// TestImageRepo exercises the artifact-library repo against PostgreSQL when
// MAMMOTH_TEST_PG_DSN points at a disposable database (`make test-pg`); it
// is skipped otherwise.
func TestImageRepo(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping image repo suite")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	repo := NewImageRepo(db)

	im := &Image{
		ID:        "img_test1",
		Name:      "rocky9-qa",
		SourceURL: "https://mirror.example/rocky9.iso",
		SHA256:    "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		Distro:    "rocky9",
		Version:   "9.4",
		State:     "fetching",
	}
	if err := repo.Create(ctx, im); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Create(ctx, im); err == nil {
		t.Fatalf("duplicate id must conflict")
	}

	got, err := repo.Get(ctx, im.ID)
	if err != nil || got.Name != "rocky9-qa" || got.State != "fetching" {
		t.Fatalf("get: %+v %v", got, err)
	}

	if _, err := repo.ReadyBySHA(ctx, im.SHA256); err == nil {
		t.Fatalf("fetching row must not satisfy ReadyBySHA")
	}

	if err := repo.SetFailed(ctx, im.ID, "checksum mismatch"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	got, _ = repo.Get(ctx, im.ID)
	if got.State != "failed" || got.Error != "checksum mismatch" {
		t.Fatalf("failed row wrong: %+v", got)
	}

	// Ready lands with size + path; ReadyBySHA serves the dedupe share.
	if err := repo.SetReady(ctx, im.ID, 1234, "/data/media/images/abc.iso"); err != nil {
		t.Fatalf("set ready: %v", err)
	}
	shared, err := repo.ReadyBySHA(ctx, im.SHA256)
	if err != nil || shared.FilePath != "/data/media/images/abc.iso" {
		t.Fatalf("ReadyBySHA: %+v %v", shared, err)
	}

	fetching, err := repo.Fetching(ctx)
	if err != nil || len(fetching) != 0 {
		t.Fatalf("no rows in fetching state now: %+v %v", fetching, err)
	}

	if n, err := repo.CountBySHA(ctx, im.SHA256); err != nil || n != 1 {
		t.Fatalf("count: %d %v", n, err)
	}

	if err := repo.Delete(ctx, im.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, im.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := repo.Delete(ctx, im.ID); err != ErrNotFound {
		t.Fatalf("second delete must be ErrNotFound, got %v", err)
	}
}
