package api

// The dd pathway's image-library integration (docs/09「镜像直写通路」):
// resolveImageRefs rewrites distro=dd specs' image.id into the machine-face
// HTTP URL (the machine streams from the engine, not the engine-local path
// the installers consume), and the file endpoint gates on state=ready with
// the written payload served back. PG-gated via MAMMOTH_TEST_PG_DSN
// (`make test-pg`), same as the resolution suite.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/gin-gonic/gin"
)

func ddImagesServer(t *testing.T) (*Server, *store.ImageRepo, func()) {
	t.Helper()
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping the dd image-library suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := store.NewImageRepo(db)
	srv := &Server{Deps: Deps{Images: repo, ExternalURL: "http://172.16.20.250:8091"}}
	cleanup := func() {
		_, _ = db.Exec(`DELETE FROM images WHERE id LIKE 'tst_dd%'`)
	}
	cleanup()
	return srv, repo, cleanup
}

func TestResolveImageRefsDDURL(t *testing.T) {
	srv, repo, cleanup := ddImagesServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := repo.Create(ctx, &store.Image{
		ID: "tst_dd_img1", Name: "thor", SourceURL: "http://up/img.gz",
		SHA256: "ab", Distro: "dd", State: "ready", FilePath: "/engine/cache/img_dd1",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := srv.resolveImageRefs(ctx, json.RawMessage(`{
	  "image": {"id": "tst_dd_img1", "distro": "dd", "format": "raw.gz"},
	  "boot": {"strategy": "pxe"}
	}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got struct {
		Image struct {
			Source   string `json:"source"`
			Checksum string `json:"checksum"`
		} `json:"image"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Image.Source != srv.ExternalURL+"/api/v1/images/tst_dd_img1/file" {
		t.Fatalf("dd source = %q, want the machine-face file URL", got.Image.Source)
	}
	if got.Image.Checksum != "sha256:ab" {
		t.Fatalf("checksum = %q, want the registration digest", got.Image.Checksum)
	}

	// Non-dd distros keep the engine-local cache path (installers read it
	// directly).
	if err := repo.Create(ctx, &store.Image{
		ID: "tst_dd_iso1", Name: "rocky", SourceURL: "http://up/r.iso",
		SHA256: "cd", Distro: "rocky9", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	// Create's INSERT carries no file_path — production lands it via
	// SetReady (the fetch worker's completion); mirror that here.
	if err := repo.SetReady(ctx, "tst_dd_iso1", 1234, "/engine/cache/img_iso1"); err != nil {
		t.Fatal(err)
	}
	out, err = srv.resolveImageRefs(ctx, json.RawMessage(`{"image": {"id": "tst_dd_iso1", "distro": "rocky9"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Image.Source != "/engine/cache/img_iso1" {
		t.Fatalf("iso source = %q, want the engine-local path", got.Image.Source)
	}
}

func TestFetchImageFileReadyGate(t *testing.T) {
	srv, repo, cleanup := ddImagesServer(t)
	defer cleanup()
	ctx := context.Background()
	dir := t.TempDir()
	payload := filepath.Join(dir, "img.img")
	if err := os.WriteFile(payload, []byte("HELLO-DD-IMAGE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, &store.Image{
		ID: "tst_dd_ok", Name: "ok", SHA256: "ab", Distro: "dd", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetReady(ctx, "tst_dd_ok", 14, payload); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, &store.Image{
		ID: "tst_dd_fetching", Name: "fetching", SHA256: "cd", Distro: "dd", State: "fetching",
	}); err != nil {
		t.Fatal(err)
	}

	// Through the strict handler wired the way production consumes it: the
	// response visitor reads/closes the Body after the handler returns.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/images/:id/file", func(c *gin.Context) {
		resp, err := srv.FetchImageFile(c, gen.FetchImageFileRequestObject{Id: c.Param("id")}) // the strict bridge passes *gin.Context AS ctx — rangeFetch keys on it
		if resp == nil && err == nil {
			return // Range path: answered on the writer already (206)
		}
		if err != nil {
			// verrStatus errors carry their HTTP status on validationError.
			status := http.StatusInternalServerError
			var ve *validationError
			if errors.As(err, &ve) {
				status = ve.status
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		_ = resp.VisitFetchImageFileResponse(c.Writer)
	})

	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/images/"+id+"/file", nil))
		return rec
	}
	if rec := get("tst_dd_ok"); rec.Code != http.StatusOK || rec.Body.String() != "HELLO-DD-IMAGE" {
		t.Fatalf("ready file: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec := get("tst_dd_fetching"); rec.Code != http.StatusConflict {
		t.Fatalf("fetching: code=%d, want 409", rec.Code)
	}
	if rec := get("tst_dd_absent"); rec.Code != http.StatusNotFound {
		t.Fatalf("absent: code=%d, want 404", rec.Code)
	}
	// Range: a resumable fetch continues at the last byte (the dd runtime's
	// wget re-issue after a dropped link).
	rangeReq := httptest.NewRequest(http.MethodGet, "/api/v1/images/tst_dd_ok/file", nil)
	rangeReq.Header.Set("Range", "bytes=6-9")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, rangeReq)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "DD-I" {
		t.Fatalf("range: code=%d body=%q, want 206 DD-I", rec.Code, rec.Body.String())
	}
}
