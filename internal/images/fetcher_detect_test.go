package images

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// TestFetcherDetectsDistro: the fetch worker sniffs the ISO after the
// digest-gated download and backfills blank distro/version rows, leaving
// registrant-provided values untouched. PG-gated via MAMMOTH_TEST_PG_DSN.
func TestFetcherDetectsDistro(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping fetcher detection suite")
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

	// 合成 rocky ISO（同 detect_test 的构建器）落到磁盘并被 httptest 托管
	isoBytes := buildTestISO(t, "Rocky-9-4-x86_64-dvd", []isoFile{
		{path: ".treeinfo", content: "[general]\nfamily = Rocky Linux\nversion = 9.4\narch = x86_64\n"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(isoBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	f := &Fetcher{Repo: repo, Events: events, Dir: dir}

	// 用例 1：注册时 distro/version 留空 → 自动回填
	im := &store.Image{
		ID: store.NewID("img"), Name: "auto", SourceURL: srv.URL + "/rocky.iso",
		SHA256: sha256hex(string(isoBytes)), State: "fetching",
	}
	if err := repo.Create(ctx, im); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Delete(ctx, im.ID) })
	f.process(ctx, im)

	got, err := repo.Get(ctx, im.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "ready" {
		t.Fatalf("state = %q, want ready", got.State)
	}
	if got.Distro != "rocky" || got.Version != "9.4" {
		t.Errorf("detected %q/%q, want rocky/9.4", got.Distro, got.Version)
	}

	// 用例 2：注册者自带 distro → 不被覆盖
	im2 := &store.Image{
		ID: store.NewID("img"), Name: "manual", SourceURL: srv.URL + "/rocky.iso",
		SHA256: sha256hex(string(isoBytes)), Distro: "my-own", State: "fetching",
	}
	if err := repo.Create(ctx, im2); err != nil {
		t.Fatalf("create im2: %v", err)
	}
	t.Cleanup(func() { _ = repo.Delete(ctx, im2.ID) })
	f.process(ctx, im2)
	got2, err := repo.Get(ctx, im2.ID)
	if err != nil {
		t.Fatalf("get im2: %v", err)
	}
	if got2.Distro != "my-own" {
		t.Errorf("distro = %q, want registrant's my-own", got2.Distro)
	}
}
