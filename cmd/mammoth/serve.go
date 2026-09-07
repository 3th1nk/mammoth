package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/api"
	"github.com/3th1nk/mammoth/internal/bmc"
	bmccompat "github.com/3th1nk/mammoth/internal/bmc/compat"
	"github.com/3th1nk/mammoth/internal/bmc/fake"
	ipmidrv "github.com/3th1nk/mammoth/internal/bmc/ipmi"
	"github.com/3th1nk/mammoth/internal/bmc/redfish"
	"github.com/3th1nk/mammoth/internal/config"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
	"github.com/3th1nk/mammoth/internal/version"
)

// serve wires the selected facets and blocks until shutdown.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	mode := fs.String("mode", "", "runtime facet: all | api | runner | builder | prober (default from MAMMOTH_MODE)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if *mode != "" {
		if m := config.Mode(*mode); !m.Valid() {
			return fmt.Errorf("invalid --mode %q", *mode)
		} else {
			cfg.Mode = m
		}
	}

	logger := obs.NewLogger(cfg.LogLevel, cfg.LogFormat)
	ctx := obs.IntoContext(mustContext(), logger)
	logger.InfoContext(ctx, "mammoth starting",
		"mode", cfg.Mode, "version", version.Version, "commit", version.Commit)

	// Tracing: no-op exporter unless OTLP endpoint is configured (D6).
	stopTracing, err := obs.SetupTracing(ctx, cfg.OTELExporterEndpoint)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() { _ = stopTracing(context.Background()) }()

	// Storage: the single runtime-strong dependency.
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}

	metrics := obs.NewMetrics(nil)

	// Table queue on the same database: task state and queue messages share
	// one store, one transaction domain (docs/10-tech-stack.md D3).
	tq := queue.NewPG(db, queue.Options{
		MaxReceiveCount: cfg.QueueMaxReceiveCount,
		RetryBackoff:    cfg.QueueRetryBackoff,
	})

	// Repositories.
	credRepo := store.NewCredentialRepo(db)
	machineRepo := store.NewMachineRepo(db)
	jobRepo := store.NewJobRepo(db)
	eventRepo := store.NewEventRepo(db)

	// Credential crypto (required to touch credentials at all).
	crypto, err := store.NewSecretCrypto(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}

	// BMC drivers: fake always available (optionally slowed to exercise
	// heartbeat/lease/reaper paths); redfish/ipmi per config.
	registry := bmc.NewRegistry(metrics, cfg.BMCTimeout)
	fakeDriver := fake.New()
	if d := getenv("MAMMOTH_FAKE_BMC_DELAY", ""); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			fakeDriver.Delay = dur
		}
	}
	registry.Register(fakeDriver)
	registry.Register(redfish.New(cfg.BMCTLSInsecure, cfg.BMCTimeout))
	registry.Register(ipmidrv.New(cfg.IPMIInterface, cfg.BMCTimeout))

	// Vendor compatibility matrix: embedded defaults, optionally extended
	// from a mounted directory (docs/compat/README.md).
	compatReg, err := bmccompat.LoadDir(getenv("MAMMOTH_COMPAT_DIR", ""))
	if err != nil {
		logger.WarnContext(ctx, "compat matrix override failed to load, using embedded defaults",
			"err", err.Error())
		if compatReg, err = bmccompat.Default(); err != nil {
			return err
		}
	}

	deps := api.Deps{
		Credentials: credRepo,
		Machines:    machineRepo,
		Jobs:        jobRepo,
		Events:      eventRepo,
		Crypto:      crypto,
		BMC:         registry,
		Queue:       tq,
		Metrics:     metrics,
		Logger:      logger,
		Visibility:  cfg.VisibilityTimeout,
	}

	errCh := make(chan error, 4)

	// API facet (control plane): HTTP + reaper.
	if cfg.Mode.RunsAPI() {
		engine := api.New(deps, cfg.APIToken)
		srv := &http.Server{Addr: cfg.HTTPAddr, Handler: engine}
		ln, lerr := net.Listen("tcp", cfg.HTTPAddr)
		if lerr != nil {
			return fmt.Errorf("listen %s: %w", cfg.HTTPAddr, lerr)
		}
		logger.InfoContext(ctx, "api listening", "addr", cfg.HTTPAddr)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()

		reaper := &provision.Reaper{
			Jobs:   jobRepo,
			Events: eventRepo,
			Opts: provision.ReaperOptions{
				Interval:       cfg.ReaperInterval,
				HeartbeatLimit: cfg.HeartbeatTimeout,
				IdempotencyTTL: cfg.IdempotencyTTL,
			},
			Logger: logger,
		}
		go func() { _ = reaper.Run(ctx) }()
	}

	// Runner facet (execution plane).
	if cfg.Mode.RunsRunner() {
		exec := &provision.Executor{
			Machines:    machineRepo,
			Credentials: credRepo,
			Jobs:        jobRepo,
			Events:      eventRepo,
			Crypto:      crypto,
			BMC:         registry,
			Compat:      compatReg,
		}
		runner := provision.NewRunner(tq, jobRepo, exec, metrics, provision.RunnerOptions{
			Concurrency:     cfg.RunnerConcurrency,
			PollInterval:    cfg.QueuePollInterval,
			Visibility:      cfg.VisibilityTimeout,
			HeartbeatEvery:  cfg.HeartbeatInterval,
			MaxTaskAttempts: cfg.RunnerMaxTaskAttempts,
		}, logger)
		go func() {
			if err := runner.Run(ctx); err != nil {
				errCh <- err
			}
		}()
	}

	// Builder / prober facets: declared in the architecture, arriving with
	// the install pipeline (M3) and in-band probes (M2).
	if cfg.Mode == config.ModeBuilder {
		return errors.New("builder facet arrives with M3 (media assembly); see docs/09-roadmap.md")
	}
	if cfg.Mode == config.ModeProber {
		return errors.New("prober facet arrives with M2 (inband_ssh probe); see docs/09-roadmap.md")
	}

	select {
	case <-ctx.Done():
		logger.InfoContext(ctx, "mammoth shutting down")
		return nil
	case err := <-errCh:
		return err
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
