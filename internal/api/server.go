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
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/render"
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
	Logs        *store.TaskLogRepo
	Crypto      *store.SecretCrypto
	BMC         *bmc.Registry
	Render      *render.Registry
	Webhooks    *store.WebhookRepo
	Queue       queue.TaskQueue
	Metrics     *obs.Metrics
	Logger      *slog.Logger

	// Netboot resolves pending boot entries for the machine-face script
	// endpoint (nil-safe: the endpoint degrades to the exit fallback).
	Netboot netboot.Resolver
	// NetbootRepo is the direct registry access for the file endpoint.
	NetbootRepo *store.NetbootRepo
	// Pending feeds the zero-registration sightings surface (nil-safe: the
	// endpoints degrade to empty/404).
	Pending *store.PendingRepo
	// Enroll carries the zero-registration enrollment surface (nil when
	// MAMMOTH_PXE_ENROLL is off): the shared boot tree descriptor for the
	// script branch and the file endpoint, and the token the report
	// endpoint expects (a mismatch is indistinguishable from off).
	Enroll *Enrollment
	// MediaDir hosts the per-task netboot boot trees (MediaDir/netboot).
	MediaDir string
	// ExternalURL is the base machines reach this server on (script URLs).
	ExternalURL string
	// BootStrategyDefault / NetbootEnabled report the boot carrier surface
	// in capabilities (empty/false when the netboot path is not configured).
	BootStrategyDefault string
	NetbootEnabled      bool
	// WindowsInstallSMBUNC reports that the deployment SMB export for the
	// windows wimboot carrier is configured (capabilities surface and the
	// windows PXE submission gate).
	WindowsInstallSMBUNC bool
	// WindowsAgentInstaller reports that the windows apply-image pathway is
	// usable on this deployment (PXE on + the alpine extended ISO pool
	// configured) — surfaced in capabilities and gating nothing by itself
	// (the pool absence fails at prepare with a classified error).
	WindowsAgentInstaller bool
	// BiosConfirmRequired gates set_bios_attributes submissions (two-stage
	// confirmation, docs/07-bmc.md §6); surfaced in capabilities.
	BiosConfirmRequired bool
	// EraseConfirmRequired gates erase_drives submissions (two-stage
	// confirmation, docs/07-bmc.md §6.2); surfaced in capabilities.
	EraseConfirmRequired bool

	// Images is the artifact-library registration store (nil-safe: the
	// image endpoints degrade to a configured-off problem).
	Images *store.ImageRepo
	// ImagesDir is the content-addressed cache root the fetch worker fills
	// (MediaDir/images); the delete handler confines its reclamation here.
	ImagesDir string

	// Config is the effective-configuration snapshot served by GET /config
	// (docs/04 §A6): env-keyed, stringified, secrets masked to "***" and
	// unset keys null by the denylist carried in ConfigRedacted. Computed
	// once at startup — configuration is process-lifetime and the endpoint
	// is read-only by design.
	Config         map[string]interface{}
	ConfigRedacted []string

	// Visibility is the lease window used when re-enqueueing retried tasks.
	Visibility time.Duration
}

// Server implements gen.StrictServerInterface.
type Server struct {
	Deps
}

// Enrollment is the zero-registration surface (docs/09-roadmap.md): the
// shared boot tree descriptor (Token fixed "enroll"; Extra carries the file
// grants the enroll-file endpoint serves) plus the token the report
// endpoint expects.
type Enrollment struct {
	Token string
	Tree  *netboot.Entry
}

// New builds the gin engine: observability middleware, /metrics, then the
// contract routes (including /healthz and /readyz, which bearer auth skips).
func New(d Deps, apiToken string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), RequestID(), ClientIP(), OTelSpan(), MetricsMiddleware(d.Metrics))

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
	// Boot-tree subtrees ride multi-segment paths (/netboot/files/<token>/
	// apks/x86_64/*.apk — the probe's apk repository) the generated
	// single-param route can't match — manual catch-alls share the strict
	// handler; allowlisting is enforced inside it (entryGrants). The shared
	// pool trees have their own catch-all below (content-addressed, no
	// per-task grant to enforce).
	for _, sub := range []string{"apks", "iso"} {
		sub := sub
		router.GET("/netboot/files/:token/"+sub+"/*rest", func(c *gin.Context) {
			req := gen.FetchNetbootFileRequestObject{
				Token: c.Param("token"),
				File:  sub + c.Param("rest"),
			}
			resp, err := srv.FetchNetbootFile(c.Request.Context(), req)
			if err != nil {
				writeError(c, err)
				return
			}
			_ = resp.VisitFetchNetbootFileResponse(c.Writer)
		})
	}
	// Per-MAC GRUB config over HTTP — the external-PXE escape hatch's
	// dynamic half (docs/operations.md §pxe-external): the site TFTP serves
	// a static trampoline that configfiles (http,mammoth)/netboot/grub/<mac>
	// with grub's ${net_default_mac} expansion, landing here where the same
	// render as the builtin TFTP hook applies (entry → config, none → exit
	// to disk). Public like the other /netboot machine-face routes.
	router.GET("/netboot/grub/:mac", func(c *gin.Context) {
		srv.serveGrubByMAC(c)
	})
	// Shared pool trees (content-addressed by the image sha256, docs §3.3):
	// the tree holds public distro content — the official ISO unpack plus
	// mammoth's pool signing key deb — so the sha in the path is an address,
	// not a credential; per-task answers and callbacks stay behind their
	// unguessable tokens on /render/{token}.
	router.GET("/netboot/store/:sha/*rest", func(c *gin.Context) {
		if err := srv.fetchPoolStoreFile(c, c.Param("sha"), c.Param("rest")); err != nil {
			writeError(c, err)
		}
	})
	// The same single-param limitation hits the answer scripts on the netboot
	// carrier: hooks are named run/mammoth/*.sh (multi-segment) and the
	// generated /render/{token}/{file} route can't match them — the installer's
	// hook fetch (early/late_command) 401s into the NoRoute auth wall and the
	// completion callback never fires (real-hardware: install ran to the end,
	// then hung at the first hook fetch).
	// Diagnostics upload: the installer environment ships setup logs back
	// (windows startnet curls the Panther logs after a failed launch) —
	// same machine-face credentialing as the rest of /render.
	router.POST("/render/:token/diag/:name", func(c *gin.Context) {
		srv.UploadDiag(c)
	})
	router.GET("/render/:token/run/*rest", func(c *gin.Context) {
		req := gen.FetchAnswerFileRequestObject{
			Token: c.Param("token"),
			File:  "run" + c.Param("rest"),
		}
		resp, err := srv.FetchAnswerFile(c.Request.Context(), req)
		if err != nil {
			writeError(c, err)
			return
		}
		_ = resp.VisitFetchAnswerFileResponse(c.Writer)
	})
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
	out := gen.Capabilities{
		Version:   version.Version,
		Resources: []string{"credentials", "machines", "jobs", "tasks"},
	}
	var distros []gen.DistroSupport
	// Distro support matrix surface (docs/06-install-pipeline.md §5).
	for _, name := range s.Render.Distros() {
		if d, err := s.Render.For(name); err == nil {
			pxe := render.PXESupport(d)
			entry := gen.DistroSupport{
				Name:                 name,
				KeepPartitionSupport: gen.DistroSupportKeepPartitionSupport(d.KeepPartitionSupport()),
				PxeSupport:           (*gen.DistroSupportPxeSupport)(&pxe),
			}
			if family := render.FamilyOf(d); family != "" {
				entry.Family = (*gen.DistroSupportFamily)(&family)
			}
			distros = append(distros, entry)
		}
	}
	out.Distros = &distros
	// Boot carrier surface (docs/06-install-pipeline.md §3): only reported
	// when set — an older runner pair stays contract-compatible.
	if s.BootStrategyDefault != "" {
		out.BootStrategyDefault = (*gen.CapabilitiesBootStrategyDefault)(&s.BootStrategyDefault)
	}
	if s.NetbootEnabled {
		out.NetbootEnabled = &s.NetbootEnabled
	}
	if s.WindowsInstallSMBUNC {
		out.WindowsSmbShare = &s.WindowsInstallSMBUNC
	}
	if s.WindowsAgentInstaller {
		out.WindowsAgentInstaller = &s.WindowsAgentInstaller
	}
	biosConfirm := "required"
	if !s.BiosConfirmRequired {
		biosConfirm = "optional"
	}
	out.BiosSetConfirm = (*gen.CapabilitiesBiosSetConfirm)(str(biosConfirm))
	eraseConfirm := "required"
	if !s.EraseConfirmRequired {
		eraseConfirm = "optional"
	}
	out.DriveEraseConfirm = (*gen.CapabilitiesDriveEraseConfirm)(str(eraseConfirm))

	return gen.GetCapabilities200JSONResponse(out), nil
}

// GetConfig serves the redacted effective-configuration snapshot
// (docs/04 §A6): the deployment's env as configured, stringified, with
// authenticating material and internal endpoints masked. The snapshot is
// computed once at startup; there is deliberately no write path —
// configuration is deployment-owned.
func (s *Server) GetConfig(ctx context.Context, _ gen.GetConfigRequestObject) (gen.GetConfigResponseObject, error) {
	out := gen.ConfigSnapshot{
		Config:   make(map[string]interface{}, len(s.Config)),
		Redacted: make([]string, 0, len(s.ConfigRedacted)),
	}
	for k, v := range s.Config {
		out.Config[k] = v
	}
	out.Redacted = append(out.Redacted, s.ConfigRedacted...)
	return gen.GetConfig200JSONResponse(out), nil
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
