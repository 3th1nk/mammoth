package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"

	"github.com/3th1nk/mammoth/internal/obs"
)

// RequestID seeds the request-scoped logger and the X-Request-Id response
// header (docs/03-api.md §4). Everything downstream logs with it.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		c.Header("X-Request-Id", id)
		ctx := obs.IntoContext(c.Request.Context(), obs.FromContext(c.Request.Context()).With(obs.FieldRequestID, id))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// BearerAuth enforces the static API token (first-version auth per
// docs/02-architecture.md §5.4; an external IdP adapter point is reserved).
// Liveness/readiness stay public so orchestrators can probe unauthenticated.
func BearerAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.FullPath() {
		case "/healthz", "/readyz":
			c.Next()
			return
		}
		if token == "" {
			problem(c, http.StatusServiceUnavailable, "SCHEMA_AUTH_UNCONFIGURED",
				"Auth not configured", "no API token configured on the server", false)
			c.Abort()
			return
		}
		const prefix = "Bearer "
		hdr := c.GetHeader("Authorization")
		if len(hdr) <= len(prefix) || hdr[:len(prefix)] != prefix || hdr[len(prefix):] != token {
			problem(c, http.StatusUnauthorized, "SCHEMA_UNAUTHORIZED", "Unauthorized",
				"missing or invalid bearer token", false)
			c.Abort()
			return
		}
		c.Next()
	}
}

// OTelSpan opens a boundary span per HTTP request with standard attributes.
func OTelSpan() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, span := obs.Tracer().Start(c.Request.Context(), "http."+c.Request.Method+" "+c.FullPath())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		span.SetAttributes(
			attribute.Int("http.status_code", c.Writer.Status()),
			attribute.String("http.route", c.FullPath()),
		)
		span.End()
	}
}

// MetricsMiddleware records request duration by route template.
func MetricsMiddleware(m *obs.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if m == nil {
			return
		}
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		m.HTTPDur.WithLabelValues(c.Request.Method, route, strconv.Itoa(c.Writer.Status())).
			Observe(time.Since(start).Seconds())
	}
}
