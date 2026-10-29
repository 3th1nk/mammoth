package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const payload = "fake distro ISO bytes — the library gates on the digest, not the content"

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// FetchToFile streams the source into the content-addressed cache behind
// the digest gate: a mismatched download never lands under the cache root.
func TestFetchToFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	dir := t.TempDir()

	good := sha256hex(payload)
	path, size, err := FetchToFile(context.Background(), srv.Client(), srv.URL+"/rocky9.iso", good, dir)
	if err != nil {
		t.Fatalf("FetchToFile: %v", err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("size: want %d, got %d", len(payload), size)
	}
	if filepath.Base(path) != good+".iso" {
		t.Fatalf("cache file must be content-addressed: %s", path)
	}
	if raw, rerr := os.ReadFile(path); rerr != nil || string(raw) != payload {
		t.Fatalf("cached bytes wrong: %v", rerr)
	}

	t.Run("digest mismatch leaves nothing behind", func(t *testing.T) {
		_, _, err := FetchToFile(context.Background(), srv.Client(), srv.URL+"/x.iso",
			sha256hex("different bytes"), dir)
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("want checksum mismatch, got %v", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("mismatched download must not cache: %d entries", len(entries))
		}
	})

	t.Run("same digest dedupes to one file", func(t *testing.T) {
		path2, _, err := FetchToFile(context.Background(), srv.Client(), srv.URL+"/other.iso", good, dir)
		if err != nil || path2 != path {
			t.Fatalf("dedupe: %v %s", err, path2)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("same digest must share one file: %d entries", len(entries))
		}
	})

	t.Run("non-http source rejected", func(t *testing.T) {
		_, _, err := FetchToFile(context.Background(), http.DefaultClient, "file:///etc/hosts", good, dir)
		if err == nil || !strings.Contains(err.Error(), "http(s)") {
			t.Fatalf("want http(s) rejection, got %v", err)
		}
	})

	t.Run("malformed digest rejected before any request", func(t *testing.T) {
		_, _, err := FetchToFile(context.Background(), http.DefaultClient, srv.URL+"/x.iso", "xyz", dir)
		if err == nil || !strings.Contains(err.Error(), "sha256 hex") {
			t.Fatalf("want digest rejection, got %v", err)
		}
	})
}

// TestFetchToFilePreStagedCache pins the upfront dedupe: bytes the cache
// already holds under their digest name resolve without touching the
// source at all (pre-staged media, re-registrations).
func TestFetchToFilePreStagedCache(t *testing.T) {
	dir := t.TempDir()
	cached := "pre-staged ISO bytes"
	digest := sha256hex(cached)
	final := filepath.Join(dir, digest+".iso")
	if err := os.WriteFile(final, []byte(cached), 0o644); err != nil {
		t.Fatalf("pre-stage: %v", err)
	}

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("must not be fetched"))
	}))
	defer srv.Close()

	path, size, err := FetchToFile(context.Background(), srv.Client(), srv.URL+"/ubuntu.iso", digest, dir)
	if err != nil {
		t.Fatalf("FetchToFile: %v", err)
	}
	if hits != 0 {
		t.Fatalf("source must not be requested when the digest is already cached, got %d hits", hits)
	}
	if path != final {
		t.Fatalf("path: want %s, got %s", final, path)
	}
	if size != int64(len(cached)) {
		t.Fatalf("size: want %d, got %d", len(cached), size)
	}
	if raw, rerr := os.ReadFile(final); rerr != nil || string(raw) != cached {
		t.Fatalf("cached bytes disturbed: %v", rerr)
	}
}
