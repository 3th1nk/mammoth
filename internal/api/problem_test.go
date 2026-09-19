package api

// writeError mapping regression: provision's classified triple has no
// Code() method, so before the explicit AsClassified branch every
// install-plan rejection degraded to the generic 500 with zero logging —
// the endpoint's first real consumer (windows2019 on the 2288H window,
// 2026-09-20) chased a malformed body through that blind spot.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/windows"
)

func TestWriteErrorMapsClassified(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reg := render.NewRegistry()
	if err := reg.Register(windows.New("windows2019")); err != nil {
		t.Fatal(err)
	}
	// distro-less spec → provision.classifiedErr(SCHEMA_INVALID_SPEC)
	_, err := provision.PlanInstall(context.Background(),
		json.RawMessage(`{"storage":{}}`), nil, reg)
	if err == nil {
		t.Fatal("expected SCHEMA_INVALID_SPEC from distro-less spec")
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/install-plan", nil)
	writeError(c, err)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("classified rejection status = %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":"SCHEMA_INVALID_SPEC"`) {
		t.Fatalf("body missing SCHEMA_INVALID_SPEC: %s", w.Body.String())
	}

	// unknown errors still degrade to the generic 500
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest("POST", "/x", nil)
	writeError(c2, errors.New("boom"))
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("unknown error status = %d, want 500", w2.Code)
	}
}
