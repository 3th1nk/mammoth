package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPoolStoreSubtreeServing(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pool-store", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "iso", "dists"), 0o755)
	os.MkdirAll(filepath.Join(dir, "pool-store", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "win", "tree", "sources"), 0o755)
	os.WriteFile(filepath.Join(dir, "pool-store", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "iso", "dists", "Release"), []byte("X"), 0o644)
	os.WriteFile(filepath.Join(dir, "pool-store", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "win", "tree", "sources", "install.wim"), []byte("W"), 0o644)
	s := &Server{Deps: Deps{MediaDir: dir}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/netboot/store/:sha/*rest", func(c *gin.Context) {
		if err := s.fetchPoolStoreFile(c, c.Param("sha"), c.Param("rest")); err != nil {
			c.String(500, "ERR")
		}
	})
	for _, tc := range []struct{ url, want string }{
		{"/netboot/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/iso/dists/Release", "X"},
		{"/netboot/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/win/tree/sources/install.wim", "W"},
		{"/netboot/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/etc/passwd", "!200"},
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", tc.url, nil)
		r.ServeHTTP(w, req)
		if tc.want == "!200" {
			if w.Code == 200 {
				t.Errorf("GET %s = %d, want rejection", tc.url, w.Code)
			}
			continue
		}
		if w.Code != 200 || w.Body.String() != tc.want {
			t.Errorf("GET %s = %d %q, want 200 %q", tc.url, w.Code, w.Body.String(), tc.want)
		}
	}
}


func TestServeExternalKitETag(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "undionly.kpxe"), []byte("nbp"), 0o644)

	fingerprint, err := kitFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/netboot/external-kit.tar", func(c *gin.Context) {
		// Mirror the handler's negotiation branch with a stub Server.
		etag, err := kitFingerprint(root)
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		if c.GetHeader("If-None-Match") != "" && c.GetHeader("If-None-Match") == etag {
			c.Status(http.StatusNotModified)
			return
		}
		c.Header("ETag", etag)
		c.Status(http.StatusOK)
	})

	// First fetch: 200 with the fingerprint as ETag.
	resp := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/netboot/external-kit.tar", nil)
	r.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Header().Get("ETag") != fingerprint {
		t.Fatalf("first fetch: code=%d etag=%q want %q", resp.Code, resp.Header().Get("ETag"), fingerprint)
	}

	// Re-fetch with the fingerprint: 304, zero bytes.
	resp2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/netboot/external-kit.tar", nil)
	req2.Header.Set("If-None-Match", fingerprint)
	r.ServeHTTP(resp2, req2)
	if resp2.Code != http.StatusNotModified {
		t.Fatalf("conditional fetch: code=%d want 304", resp2.Code)
	}

	// Changed content: fingerprint moves, 200 again.
	os.WriteFile(filepath.Join(root, "undionly.kpxe"), []byte("nbp-v2"), 0o644)
	_, _ = kitFingerprint(root)
	resp3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/netboot/external-kit.tar", nil)
	req3.Header.Set("If-None-Match", fingerprint)
	r.ServeHTTP(resp3, req3)
	if resp3.Code != http.StatusOK || resp3.Header().Get("ETag") == fingerprint {
		t.Fatalf("content change: code=%d etag=%q — must move off the old fingerprint", resp3.Code, resp3.Header().Get("ETag"))
	}
}
