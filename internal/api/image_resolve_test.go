package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// Artifact-library resolution: image.id rewrites into the checksum-gated
// cache source at submit; the registration row is snapshot material, not a
// runtime dependency. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestResolveImageRefs(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping image resolution suite")
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
	sha := strings.Repeat("ab", 32)
	im := &store.Image{
		ID:        store.NewID("tst_img_"),
		SourceURL: "https://mirror.example/rocky9.iso",
		SHA256:    sha,
		State:     "fetching",
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM images WHERE id = $1`, im.ID) })
	if err := srv.Images.Create(ctx, im); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	spec := func(image map[string]any) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"image": image})
		return raw
	}

	t.Run("passthrough without id", func(t *testing.T) {
		raw := spec(map[string]any{"distro": "rocky9", "source": "https://m/x.iso"})
		got, err := srv.resolveImageRefs(ctx, raw)
		if err != nil || string(got) != string(raw) {
			t.Fatalf("passthrough: %v %s", err, got)
		}
	})

	t.Run("fetching image rejects the submission", func(t *testing.T) {
		raw := spec(map[string]any{"distro": "rocky9", "id": im.ID})
		if _, err := srv.resolveImageRefs(ctx, raw); err == nil {
			t.Fatalf("fetching image must reject")
		}
	})

	if err := srv.Images.SetReady(ctx, im.ID, 4096, "/data/media/images/"+sha+".iso"); err != nil {
		t.Fatalf("set ready: %v", err)
	}

	t.Run("id resolves to path + digest and drops the id", func(t *testing.T) {
		raw := spec(map[string]any{"distro": "rocky9", "id": im.ID})
		got, err := srv.resolveImageRefs(ctx, raw)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		var out struct {
			Image struct {
				Source   string `json:"source"`
				Checksum string `json:"checksum"`
				ID       string `json:"id"`
			} `json:"image"`
		}
		if json.Unmarshal(got, &out) != nil {
			t.Fatalf("unmarshal resolved: %s", got)
		}
		if out.Image.Source != "/data/media/images/"+sha+".iso" {
			t.Fatalf("source not rewritten: %q", out.Image.Source)
		}
		if out.Image.Checksum != "sha256:"+sha {
			t.Fatalf("checksum not stamped: %q", out.Image.Checksum)
		}
		if out.Image.ID != "" {
			t.Fatalf("id must not survive resolution: %q", out.Image.ID)
		}
	})

	t.Run("conflicting explicit checksum rejects", func(t *testing.T) {
		raw := spec(map[string]any{"distro": "rocky9", "id": im.ID, "checksum": "sha256:" + strings.Repeat("cd", 32)})
		if _, err := srv.resolveImageRefs(ctx, raw); err == nil {
			t.Fatalf("conflicting checksum must reject")
		}
	})

	t.Run("unknown id rejects", func(t *testing.T) {
		raw := spec(map[string]any{"distro": "rocky9", "id": "img_nope"})
		if _, err := srv.resolveImageRefs(ctx, raw); err == nil {
			t.Fatalf("unknown id must reject")
		}
	})

	t.Run("id + source is mutually exclusive", func(t *testing.T) {
		raw := spec(map[string]any{"distro": "rocky9", "id": im.ID, "source": "https://m/x.iso"})
		if _, err := srv.resolveImageRefs(ctx, raw); err == nil {
			t.Fatalf("id+source must reject")
		}
	})
}
