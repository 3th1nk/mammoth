package images

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// The dd pathway's compressed-image facts (docs/09「镜像直写通路」): a gzip
// artifact lands ready with its compression format and the UNCOMPRESSED
// digest/span the runtime verifies — not the compressed file's registration
// digest. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestFetcherLandsCompressionFacts(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping fetcher compression suite")
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

	payload := bytes.Repeat([]byte("mammoth-dd-\x00payload"), 4096)
	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	gzBytes := gzBuf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	f := &Fetcher{Repo: repo, Events: events, Dir: dir}

	im := &store.Image{
		ID: store.NewID("img"), Name: "jetson.img.gz", SourceURL: srv.URL + "/disk.img.gz",
		SHA256: sha256hex(string(gzBytes)), State: "fetching",
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
		t.Fatalf("state = %s (err=%s)", got.State, got.Error)
	}
	if got.Format != FormatRawGZ {
		t.Fatalf("format = %q, want raw.gz", got.Format)
	}
	want := sha256.Sum256(payload)
	if got.RawSHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("raw digest = %s, want the uncompressed payload's", got.RawSHA256)
	}
	if got.RawSizeBytes != int64(len(payload)) {
		t.Fatalf("raw size = %d, want %d", got.RawSizeBytes, len(payload))
	}
	// the registered face stays the compressed file's — the fetch gate's truth
	if got.SizeBytes != int64(len(gzBytes)) || got.SHA256 != sha256hex(string(gzBytes)) {
		t.Fatalf("registered face drifted: sha=%s size=%d", got.SHA256, got.SizeBytes)
	}
}
