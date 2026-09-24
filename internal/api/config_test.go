package api

import (
	"context"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
)

// GET /config is a pure projection of the startup snapshot (docs/04 §A6):
// whatever the serve layer computed is echoed verbatim, and a server built
// without the snapshot (test harnesses) degrades to an empty one rather
// than panicking.
func TestGetConfigEchoesSnapshot(t *testing.T) {
	s := &Server{Deps{Config: map[string]interface{}{
		"MAMMOTH_HTTP_ADDR": ":8080",
		"MAMMOTH_API_TOKEN": "***",
		"MAMMOTH_PXE_MODE":  nil,
	}, ConfigRedacted: []string{"MAMMOTH_API_TOKEN"}}}

	resp, err := s.GetConfig(context.Background(), gen.GetConfigRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := resp.(gen.GetConfig200JSONResponse)
	if !ok {
		t.Fatalf("response type = %T", resp)
	}
	if out.Config["MAMMOTH_HTTP_ADDR"] != ":8080" || out.Config["MAMMOTH_PXE_MODE"] != nil {
		t.Fatalf("snapshot not echoed verbatim: %v", out.Config)
	}
	if len(out.Redacted) != 1 || out.Redacted[0] != "MAMMOTH_API_TOKEN" {
		t.Fatalf("redacted list = %v", out.Redacted)
	}

	empty := &Server{}
	r2, err := empty.GetConfig(context.Background(), gen.GetConfigRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	e := r2.(gen.GetConfig200JSONResponse)
	if e.Config == nil || len(e.Config) != 0 {
		t.Fatalf("nil snapshot must degrade to an empty map, got %v", e.Config)
	}
}
