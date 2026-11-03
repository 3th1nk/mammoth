package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/store"
)

// The machine-face file routes serve multi-hundred-MB initrds; a dropped
// connection must resume at the last byte (Range → 206), not restart from
// zero. Covered without PostgreSQL: the pool-tree handler and the enroll
// strict handler take no repo. The per-task boot tree needs ByToken, so its
// suite is PG-gated like the other repo-backed suites.

const rangeStoreSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func rangeTestPoolTree(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	iso := filepath.Join(dir, "pool-store", rangeStoreSHA, "iso", "install")
	if err := os.MkdirAll(iso, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iso, "initrd.gz"), []byte("0123456789ABCDEF"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Server{Deps: Deps{MediaDir: dir}}
}

func TestPoolStoreRangeServing(t *testing.T) {
	s := rangeTestPoolTree(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/netboot/store/:sha/*rest", func(c *gin.Context) {
		if err := s.fetchPoolStoreFile(c, c.Param("sha"), c.Param("rest")); err != nil {
			writeError(c, err)
		}
	})
	url := func() string {
		return "/netboot/store/" + rangeStoreSHA + "/iso/install/initrd.gz"
	}

	// No Range: the whole body at 200 with Accept-Ranges advertised.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url(), nil))
	if w.Code != http.StatusOK || w.Body.String() != "0123456789ABCDEF" {
		t.Fatalf("plain GET = %d %q, want 200 full body", w.Code, w.Body.String())
	}
	if w.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("plain GET missing Accept-Ranges header, got %q", w.Header().Get("Accept-Ranges"))
	}

	// Mid-file range: 206 with exactly the requested bytes.
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url(), nil)
	req.Header.Set("Range", "bytes=4-9")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "456789" {
		t.Fatalf("bytes=4-9 = %d %q, want 206 %q", w.Code, w.Body.String(), "456789")
	}
	if got, want := w.Header().Get("Content-Range"), "bytes 4-9/16"; got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}

	// Suffix range — the resume-at-the-end shape senders use after a drop.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, url(), nil)
	req.Header.Set("Range", "bytes=-4")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "CDEF" {
		t.Fatalf("bytes=-4 = %d %q, want 206 %q", w.Code, w.Body.String(), "CDEF")
	}

	// Unsatifiable range: 416, never a silent whole-body 200.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, url(), nil)
	req.Header.Set("Range", "bytes=99-")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("bytes=99- = %d, want 416", w.Code)
	}

	// Malformed bytes-range syntax and unknown units both answer 416 in
	// Go's ServeContent (stricter than RFC 9110's "MAY ignore"); real
	// resumable clients only ever send bytes= specs.
	for _, bad := range []string{"bytes=abc", "blobs=0-1"} {
		w = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, url(), nil)
		req.Header.Set("Range", bad)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("Range %q = %d, want 416", bad, w.Code)
		}
	}

	// The sha slot gate holds for Range requests too.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/netboot/store/deadbeef/iso/install/initrd.gz", nil)
	req.Header.Set("Range", "bytes=0-")
	r.ServeHTTP(w, req)
	if w.Code == http.StatusPartialContent {
		t.Fatalf("non-sha address served a range: %d", w.Code)
	}
}

// TestEnrollFileRangeServing drives the strict handler the way the generated
// wrapper does — the *gin.Context AS ctx contract — so the Range branch
// (write 206, return nil response) is exercised end to end.
func TestEnrollFileRangeServing(t *testing.T) {
	dir := t.TempDir()
	treeDir := filepath.Join(dir, "netboot", "enroll")
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(treeDir, "initrd"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{Deps: Deps{
		MediaDir: dir,
		Enroll:   &Enrollment{Token: "enroll-tok", Tree: &netboot.Entry{Token: "enroll", Kernel: "vmlinuz", Initrd: "initrd"}},
	}}

	call := func(rangeHeader string) (*httptest.ResponseRecorder, gen.FetchNetbootEnrollFileResponseObject) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/netboot/enroll/files/enroll-tok/initrd", nil)
		if rangeHeader != "" {
			c.Request.Header.Set("Range", rangeHeader)
		}
		resp, err := s.FetchNetbootEnrollFile(c, gen.FetchNetbootEnrollFileRequestObject{Name: "initrd"})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		return w, resp
	}

	// Range fetch: 206 written on the gin writer, nil response returned —
	// the generated wrapper must then leave the response alone.
	w, resp := call("bytes=2-5")
	if resp != nil {
		t.Fatalf("Range fetch returned a response object %T — the wrapper would re-serve 200 over it", resp)
	}
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" {
		t.Fatalf("bytes=2-5 = %d %q, want 206 %q", w.Code, w.Body.String(), "2345")
	}

	// Plain fetch: the whole-body response object comes back for the
	// visitor — visiting it must land the full body at 200.
	w, resp = call("")
	if resp == nil {
		t.Fatal("plain fetch returned nil response — visitor path lost")
	}
	if err := resp.VisitFetchNetbootEnrollFileResponse(w); err != nil {
		t.Fatalf("visit plain response: %v", err)
	}
	if w.Code != http.StatusOK || w.Body.String() != "0123456789" {
		t.Fatalf("plain fetch = %d %q, want 200 full body", w.Code, w.Body.String())
	}
}

// TestBootTreeFileRangeServing covers the per-task boot tree (the heavy
// path: per-distro kernel+initrd): Range through the strict handler both as
// the generated single-file route passes it and through the subtree
// catch-all shape (which must survive the nil response). PG-gated —
// ByToken needs netboot_entries.
func TestBootTreeFileRangeServing(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping boot-tree range suite")
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
	const mac, token = "52:54:00:aa:bb:dd", "tst_range_tok"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM netboot_entries WHERE mac = $1`, mac)
	})

	dir := t.TempDir()
	tree := filepath.Join(dir, "netboot", token)
	if err := os.MkdirAll(filepath.Join(tree, "apks", "x86_64"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "initrd"), []byte("0123456789ABCDEF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "apks", "x86_64", "apk-tools.apk"), []byte("PKGG"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := store.NewNetbootRepo(db)
	if err := repo.Upsert(ctx, &store.NetbootEntry{
		MAC: mac, TaskID: "tst_range_task", MachineID: "tst_range_machine", Token: token,
		Kind: "install", Kernel: "vmlinuz", Initrd: "initrd",
		Extra: map[string]string{"modloop": "dir:apks"},
	}); err != nil {
		t.Fatalf("upsert entry: %v", err)
	}

	s := &Server{Deps: Deps{MediaDir: dir, NetbootRepo: repo}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Both live shapes: the generated single-file route and the subtree
	// catch-all with the nil-response guard (server.go).
	r.GET("/netboot/files/:token/:file", func(c *gin.Context) {
		resp, err := s.FetchNetbootFile(c, gen.FetchNetbootFileRequestObject{
			Token: c.Param("token"), File: c.Param("file"),
		})
		if err != nil {
			writeError(c, err)
			return
		}
		if resp != nil {
			_ = resp.VisitFetchNetbootFileResponse(c.Writer)
		}
	})
	r.GET("/netboot/files/:token/apks/*rest", func(c *gin.Context) {
		resp, err := s.FetchNetbootFile(c, gen.FetchNetbootFileRequestObject{
			Token: c.Param("token"), File: "apks" + c.Param("rest"),
		})
		if err != nil {
			writeError(c, err)
			return
		}
		if resp != nil {
			_ = resp.VisitFetchNetbootFileResponse(c.Writer)
		}
	})

	// Single-file route, mid-file range.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/netboot/files/"+token+"/initrd", nil)
	req.Header.Set("Range", "bytes=10-")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "ABCDEF" {
		t.Fatalf("initrd bytes=10- = %d %q, want 206 %q", w.Code, w.Body.String(), "ABCDEF")
	}

	// Plain fetch stays whole-body 200 through the visitor.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/netboot/files/"+token+"/initrd", nil))
	if w.Code != http.StatusOK || w.Body.String() != "0123456789ABCDEF" {
		t.Fatalf("plain initrd fetch = %d %q, want 200 full body", w.Code, w.Body.String())
	}

	// Subtree catch-all honors Range too (the guard must not re-serve).
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/netboot/files/"+token+"/apks/x86_64/apk-tools.apk", nil)
	req.Header.Set("Range", "bytes=1-2")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "KG" {
		t.Fatalf("apk bytes=1-2 = %d %q, want 206 %q", w.Code, w.Body.String(), "KG")
	}

	// Grant rules hold under Range: ungranted names stay 404.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/netboot/files/"+token+"/../secret", nil)
	req.Header.Set("Range", "bytes=0-")
	r.ServeHTTP(w, req)
	if w.Code == http.StatusPartialContent {
		t.Fatalf("traversal under Range served: %d", w.Code)
	}
}
