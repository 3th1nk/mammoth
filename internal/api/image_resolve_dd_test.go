package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// dd pathway + compressed library image (docs/09「镜像直写通路」): the
// resolution must carry the UNCOMPRESSED digest/span (the face the runtime's
// write-stream hash and read-back see), reject a declared format conflicting
// with the sniffed compression, and inherit the format when the spec omits
// it. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestResolveImageRefsDDCompressed(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping dd resolution suite")
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
	srv := &Server{Deps: Deps{Images: store.NewImageRepo(db)}}
	sha := strings.Repeat("cd", 32)    // compressed file's registration digest
	rawSHA := strings.Repeat("ef", 32) // uncompressed image's digest

	im := &store.Image{
		ID: store.NewID("tst_img_"), SourceURL: "https://images.example/jetson.img.xz",
		SHA256: sha, Format: "raw.xz", RawSHA256: rawSHA, RawSizeBytes: 8 << 30,
		State: "fetching",
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM images WHERE id = $1`, im.ID) })
	if err := srv.Images.Create(ctx, im); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	if err := srv.Images.SetReady(ctx, im.ID, 3<<30, "/data/media/images/"+sha+".iso"); err != nil {
		t.Fatalf("set ready: %v", err)
	}

	spec := func(image map[string]any) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"image": image})
		return raw
	}
	var out struct {
		Image struct {
			Source    string `json:"source"`
			Checksum  string `json:"checksum"`
			Format    string `json:"format"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"image"`
	}

	t.Run("resolution carries the uncompressed face", func(t *testing.T) {
		got, err := srv.resolveImageRefs(ctx, spec(map[string]any{"distro": "dd", "id": im.ID}))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if err := json.Unmarshal(got, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Image.Checksum != "sha256:"+rawSHA {
			t.Fatalf("checksum = %s, want the uncompressed digest", out.Image.Checksum)
		}
		if out.Image.SizeBytes != 8<<30 {
			t.Fatalf("size_bytes = %d, want the uncompressed span", out.Image.SizeBytes)
		}
		if !strings.Contains(out.Image.Source, "/api/v1/images/"+im.ID+"/file") {
			t.Fatalf("source = %s, want the machine-face file route", out.Image.Source)
		}
	})

	t.Run("conflicting declared format rejects", func(t *testing.T) {
		if _, err := srv.resolveImageRefs(ctx, spec(map[string]any{
			"distro": "dd", "id": im.ID, "format": "raw.gz",
		})); err == nil {
			t.Fatalf("raw.gz declaration on a raw.xz image must reject")
		}
	})

	t.Run("omitted format inherits the registration's", func(t *testing.T) {
		got, err := srv.resolveImageRefs(ctx, spec(map[string]any{"distro": "dd", "id": im.ID}))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if err := json.Unmarshal(got, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Image.Format != "raw.xz" {
			t.Fatalf("format = %s, want the sniffed raw.xz", out.Image.Format)
		}
	})
}
