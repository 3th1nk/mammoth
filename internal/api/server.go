package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
	"github.com/3th1nk/mammoth/internal/version"
)

// Deps carries everything the handlers touch. The server stays stateless so
// the control plane can run multi-replica (docs/02-architecture.md §1).
type Deps struct {
	Credentials *store.CredentialRepo
	Machines    *store.MachineRepo
	Jobs        *store.JobRepo
	Events      *store.EventRepo
	Crypto      *store.SecretCrypto
	BMC         *bmc.Registry
	Queue       queue.TaskQueue
	Metrics     *obs.Metrics
	Logger      *slog.Logger

	// Visibility is the lease window used when re-enqueueing retried tasks.
	Visibility time.Duration
}

// Server implements gen.StrictServerInterface.
type Server struct {
	Deps
}

// New builds the gin engine: observability middleware, /metrics, then the
// contract routes (including /healthz and /readyz, which bearer auth skips).
func New(d Deps, apiToken string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), RequestID(), OTelSpan(), MetricsMiddleware(d.Metrics))

	srv := &Server{Deps: d}
	strict := gen.NewStrictHandlerWithOptions(srv, nil, gen.StrictGinServerOptions{
		// Handler-returned errors map onto the RFC 9457 problem space here —
		// the single choke point for NotFound/Conflict/validation/BMC codes.
		HandlerErrorFunc: writeError,
	})
	router.Use(BearerAuth(apiToken))

	if d.Metrics != nil {
		router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	}
	gen.RegisterHandlers(router, strict)

	router.NoRoute(func(c *gin.Context) {
		notFound(c, "route")
	})
	return router
}

func readyz(c *gin.Context, d Deps) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if _, _, err := d.Jobs.ListJobs(ctx, store.JobListFilter{PageSize: 1}); err != nil {
		problem(c, http.StatusServiceUnavailable, "SCHEMA_NOT_READY", "Not ready",
			"database unreachable", true)
		return
	}
	if _, err := d.Queue.Depth(ctx, provision.QueueTasks); err != nil {
		problem(c, http.StatusServiceUnavailable, "SCHEMA_NOT_READY", "Not ready",
			"queue unreachable", true)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── meta ────────────────────────────────────────────────────────────────────

func (s *Server) Healthz(ctx context.Context, _ gen.HealthzRequestObject) (gen.HealthzResponseObject, error) {
	return gen.Healthz204Response{}, nil
}

func (s *Server) Readyz(ctx context.Context, _ gen.ReadyzRequestObject) (gen.ReadyzResponseObject, error) {
	if _, _, err := s.Jobs.ListJobs(ctx, store.JobListFilter{PageSize: 1}); err != nil {
		return gen.Readyz503ApplicationProblemPlusJSONResponse(s.notReadyProblem("database unreachable")), nil
	}
	if _, err := s.Queue.Depth(ctx, provision.QueueTasks); err != nil {
		return gen.Readyz503ApplicationProblemPlusJSONResponse(s.notReadyProblem("queue unreachable")), nil
	}
	return gen.Readyz204Response{}, nil
}

func (s *Server) notReadyProblem(detail string) gen.Problem {
	retryable := true
	return gen.Problem{
		Code:      str("SCHEMA_NOT_READY"),
		Detail:    str(detail),
		Title:     "Not ready",
		Status:    503,
		Retryable: &retryable,
	}
}

func (s *Server) GetCapabilities(ctx context.Context, _ gen.GetCapabilitiesRequestObject) (gen.GetCapabilitiesResponseObject, error) {
	return gen.GetCapabilities200JSONResponse(gen.Capabilities{
		Version:   version.Version,
		Resources: []string{"credentials", "machines", "jobs", "tasks"},
	}), nil
}

// ── shared helpers ──────────────────────────────────────────────────────────

// encodeCursor / decodeCursor wrap the opaque keyset token.
func encodeCursor(c *store.Cursor) *string {
	if c == nil {
		return nil
	}
	raw, _ := json.Marshal(c)
	return str(base64.RawURLEncoding.EncodeToString(raw))
}

func decodeCursor(s *gen.Cursor) *store.Cursor {
	if s == nil || *s == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(*s))
	if err != nil {
		return nil
	}
	var c store.Cursor
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return &c
}

func orderOf(o *string) string {
	if o != nil && *o == "asc" {
		return "asc"
	}
	return "desc"
}
