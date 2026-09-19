// Package api implements the control plane HTTP surface: contract-generated
// routing and typing (internal/api/gen) + handwritten handlers here. The
// OpenAPI contract is the single source of truth; contract drift fails the
// build (docs/03-api.md §5).
package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/store"
)

// problem writes an RFC 9457 response with Mammoth extensions (code,
// retryable) — the universal error shape of this API.
func problem(c *gin.Context, status int, code, title, detail string, retryable bool) {
	c.Header("Content-Type", "application/problem+json")
	c.JSON(status, gin.H{
		"type":      fmt.Sprintf("https://mammoth.dev/errors/%s", lower(code)),
		"title":     title,
		"status":    status,
		"detail":    detail,
		"code":      code,
		"retryable": retryable,
	})
}

func lower(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if b == '_' {
			b = '-'
		}
		out = append(out, b)
	}
	return string(out)
}

// writeError maps domain errors onto the problem responses the contract
// declares. Unknown errors degrade to 500 with a generic code.
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(c, http.StatusNotFound, "SCHEMA_NOT_FOUND", "Not found", "resource not found", false)
	case errors.Is(err, store.ErrConflict):
		problem(c, http.StatusConflict, "JOB_CONFLICT", "Conflict", err.Error(), false)
	case errors.Is(err, store.ErrIdempotentHit):
		problem(c, http.StatusConflict, "JOB_IDEMPOTENCY_CONFLICT", "Idempotency key replay",
			"the Idempotency-Key was already used for a different request", false)
	default:
		var bmcErr *bmc.Error
		if errors.As(err, &bmcErr) {
			// BMC failures surfaced synchronously (console URL) map to 502.
			problem(c, http.StatusBadGateway, bmcErr.Code(), "Out-of-band operation failed",
				bmcErr.Error(), bmcErr.Retryable())
			return
		}
		var appErr interface{ Code() string }
		if errors.As(err, &appErr) {
			status := http.StatusUnprocessableEntity
			var ve *validationError
			if errors.As(err, &ve) {
				status = ve.status
			}
			problem(c, status, appErr.Code(), "Request rejected", err.Error(), false)
			return
		}
		// provision's classified triple (install-plan's LAYOUT_*/RENDER_FAILED/
		// SCHEMA_*, …) carries no Code() method — map it explicitly instead of
		// letting it degrade to a 500 that hides the actual rejection.
		if ci, ok := provision.AsClassified(err); ok {
			problem(c, http.StatusUnprocessableEntity, ci.Code, "Request rejected", ci.Message, ci.Retryable)
			return
		}
		obs.FromContext(c.Request.Context()).WarnContext(c.Request.Context(),
			"unmapped error → 500", "err", err.Error())
		problem(c, http.StatusInternalServerError, "SCHEMA_INTERNAL", "Internal error",
			"unexpected server error", false)
	}
}

// validationError is a semantic rejection carrying its HTTP status (422 by
// default; resources that declare 404 in the contract use 404).
type validationError struct {
	code   string
	msg    string
	status int
}

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Code() string  { return e.code }

func verr(code, format string, args ...any) error {
	return &validationError{code: code, msg: fmt.Sprintf(format, args...), status: http.StatusUnprocessableEntity}
}

func verrStatus(status int, code, format string, args ...any) error {
	return &validationError{code: code, msg: fmt.Sprintf(format, args...), status: status}
}

// notFound builds the standard "not found" problem for path resources.
func notFound(c *gin.Context, what string) {
	problem(c, http.StatusNotFound, "SCHEMA_NOT_FOUND", "Not found", what+" not found", false)
}
