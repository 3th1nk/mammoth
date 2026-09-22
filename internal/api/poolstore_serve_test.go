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
