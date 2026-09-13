package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/assets/pxe"
	"github.com/3th1nk/mammoth/internal/api"
	"github.com/3th1nk/mammoth/internal/bmc"
	bmccompat "github.com/3th1nk/mammoth/internal/bmc/compat"
	"github.com/3th1nk/mammoth/internal/bmc/fake"
	ipmidrv "github.com/3th1nk/mammoth/internal/bmc/ipmi"
	"github.com/3th1nk/mammoth/internal/bmc/redfish"
	"github.com/3th1nk/mammoth/internal/config"
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/mediarelay"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/nfsx"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/autoinstall"
	"github.com/3th1nk/mammoth/internal/render/kickstart"
	"github.com/3th1nk/mammoth/internal/render/preseed"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
	"github.com/3th1nk/mammoth/internal/version"
	"github.com/3th1nk/mammoth/internal/webhook"
)

// serve wires the selected facets and blocks until shutdown.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	mode := fs.String("mode", "", "runtime facet: all | api | runner | builder | prober (default from MAMMOTH_MODE)")
	envFile := fs.String("env-file", "", "dotenv file to seed the environment before reading MAMMOTH_* (also MAMMOTH_ENV_FILE); existing environment variables win")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *envFile != "" {
		_ = os.Setenv("MAMMOTH_ENV_FILE", *envFile)
	}

	cfg, err := config.Load()
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
	webhookRepo := store.NewWebhookRepo(db)
	logsRepo := store.NewTaskLogRepo(db)
	netbootRepo := store.NewNetbootRepo(db)

	// Log dual-write (docs/02-architecture.md §5.2): from here on, every log
	// line that carries task_id is also persisted for API retrieval; lines
	// without task_id just pass through the tee untouched.
	logger = obs.NewTaskLogTee(logger, logsRepo)
	ctx = obs.IntoContext(ctx, logger)

	// Credential crypto (required to touch credentials at all).
	crypto, err := store.NewSecretCrypto(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}

	// BMC drivers: fake always available (optionally slowed to exercise
	// heartbeat/lease/reaper paths); redfish/ipmi per config.
	registry := bmc.NewRegistry(metrics, cfg.BMCTimeout)
	fakeDriver := fake.New()
	var fakeInbandDelay time.Duration
	if cfg.DevFakeBMCDelay > 0 {
		fakeDriver.Delay = cfg.DevFakeBMCDelay
		fakeInbandDelay = cfg.DevFakeBMCDelay
	}
	registry.Register(fakeDriver)
	registry.Register(redfish.New(cfg.BMCTLSInsecure, cfg.BMCTimeout))
	registry.Register(ipmidrv.New(cfg.IPMIInterface, cfg.BMCTimeout))

	// In-band probe: agent-less, read-only, one connection; the whole
	// collection is bounded by the in-band timeout (docs/05-inventory.md §3).
	inbandRunner := &inbandssh.SSHRunner{DialTimeout: cfg.InbandTimeout / 3}
	inband := &inbandssh.Collector{Runner: &inbandssh.SwitchRunner{
		SSH:   inbandRunner,
		Fake:  &inbandssh.StaticRunner{Output: []byte(inbandssh.DefaultFixture)},
		Delay: fakeInbandDelay,
	}}

	// Media repository: boot ISOs land in MediaDir; MediaBaseURI exposes it
	// to BMCs (nfs://host/export base for the virtual media mount URI).
	if err := os.MkdirAll(cfg.MediaDir, 0o755); err != nil {
		return fmt.Errorf("media dir: %w", err)
	}

	// Built-in media export (default on): MediaDir is served read-only over
	// NFSv3 in-process, so the BMC mounts nfs://<this host>/... directly.
	// Disable (MAMMOTH_NFS_EXPORT=false) to use an external NFS service.
	if cfg.NFSExportEnabled {
		nfsSrv, nerr := nfsx.Start(ctx, cfg.MediaDir, cfg.NFSExportPort)
		if nerr != nil {
			// Not fatal: deployments may serve the media dir from an external
			// NFS service — they just must set MAMMOTH_MEDIA_BASE_URI.
			logger.Warn("built-in nfs export unavailable", "err", nerr.Error())
		} else {
			logger.Info("built-in nfs export serving", "dir", cfg.MediaDir, "port", cfg.NFSExportPort)
			go func() {
				if serr := nfsSrv.Wait(); serr != nil {
					logger.Warn("built-in nfs export stopped", "err", serr.Error())
				}
			}()
		}
	}

	// Media relay (optional): when set, assembled boot media is pushed to the
	// BMC-reachable export in-process — replacing out-of-band push scripts.
	// NOTE: keep the interface nil when no relay is configured — a typed-nil
	// (*Relay)(nil) makes `MediaUploader != nil` true and panics at call time.
	var mediaUploader provision.MediaUploader
	var mediaRelay *mediarelay.Relay
	if cfg.MediaRelayAddr != "" {
		relay, rerr := mediarelay.New(cfg.MediaRelayAddr, cfg.MediaRelayUser,
			cfg.MediaRelayPassword, cfg.MediaRelayDir, 10*time.Minute)
		if rerr != nil {
			return fmt.Errorf("media relay: %w", rerr)
		}
		mediaRelay = relay
	}
	if mediaRelay != nil {
		mediaUploader = mediaRelay
	}

	// Distro drivers register here; adding a distro never touches the
	// orchestration layer (docs/06-install-pipeline.md §5). "uniontechos"
	// (UOS Server V20) is anaconda-based with an RHEL-style install tree —
	// it belongs to the kickstart dialect, NOT preseed (ISO inspected:
	// AppStream/BaseOS/isolinux, no debian-installer layout).
	renderReg := render.NewRegistry()
	for _, d := range []render.OSDriver{
		kickstart.New("rocky9"),
		kickstart.New("rocky10"),
		kickstart.New("centos7"),
		kickstart.New("kylinv10"),
		kickstart.New("kylinv11"),
		kickstart.New("uniontechos"),
		autoinstall.New("ubuntu22"),
		preseed.New("debian12"),
	} {
		if err := renderReg.Register(d); err != nil {
			return err
		}
	}

	// Vendor compatibility matrix: embedded defaults, optionally extended
	// from a mounted directory (docs/compat/README.md).
	var compatReg *bmccompat.Registry
	if cfg.CompatDir != "" {
		compatReg, err = bmccompat.LoadDir(cfg.CompatDir)
		if err != nil {
			logger.WarnContext(ctx, "compat matrix override failed to load, using embedded defaults",
				"err", err.Error())
		}
	}
	if compatReg == nil {
		if compatReg, err = bmccompat.Default(); err != nil {
			return err
		}
	}

	// PXE network boot services (M7): proxyDHCP + TFTP are opt-in, bind
	// privileged ports, and must co-locate with the machine face (script
	// URLs point at this host). Unlike the NFS export there is no external
	// escape hatch — a silent downgrade would strand machines at the PXE
	// prompt — so a bind failure is fatal when explicitly enabled.
	var nbResolver netboot.Resolver
	if cfg.PXEEnabled && cfg.Mode.RunsNetboot() {
		nextServer := net.ParseIP(cfg.PXENextServer)
		nb, nerr := netboot.Start(ctx, netboot.Options{
			DHCPPort:   cfg.PXEDHCPPort,
			ProxyPort:  cfg.PXEProxyPort,
			TFTPPort:   cfg.PXETFTPPort,
			NextServer: nextServer,
			BaseURL:    strings.TrimSuffix(cfg.ExternalURL, "/"),
			NBPs:       pxe.Files,
			Log:        logger,
		})
		if nerr != nil {
			return fmt.Errorf("netboot: %w", nerr)
		}
		go func() {
			if serr := nb.Wait(); serr != nil {
				logger.Warn("netboot service stopped", "err", serr.Error())
			}
		}()
		// MAC → entry lookups for the script endpoint: store answer with a
		// small cache (firmware re-asks several times per boot).
		nbResolver = netboot.NewCachedResolver(
			netboot.ResolverFunc(func(ctx context.Context, mac string) (*netboot.Entry, error) {
				e, err := netbootRepo.ByMAC(ctx, mac)
				if errors.Is(err, store.ErrNotFound) {
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				return &netboot.Entry{
					MAC: e.MAC, TaskID: e.TaskID, Kind: e.Kind, Token: e.Token,
					Kernel: e.Kernel, Initrd: e.Initrd, KernelArgs: e.KernelArgs,
					Extra: e.Extra,
				}, nil
			}), 15*time.Second)
	}

	deps := api.Deps{
		Credentials: credRepo,
		Machines:    machineRepo,
		Jobs:        jobRepo,
		Events:      eventRepo,
		Logs:        logsRepo,
		Crypto:      crypto,
		BMC:         registry,
		Render:      renderReg,
		Webhooks:    webhookRepo,
		Queue:       tq,
		Metrics:     metrics,
		Logger:      logger,
		Visibility:  cfg.VisibilityTimeout,

		Netboot:             nbResolver,
		NetbootRepo:         netbootRepo,
		MediaDir:            cfg.MediaDir,
		ExternalURL:         strings.TrimSuffix(cfg.ExternalURL, "/"),
		BootStrategyDefault: "virtual_media",
		NetbootEnabled:      cfg.PXEEnabled && cfg.Mode.RunsNetboot(),
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
			Jobs:     jobRepo,
			Events:   eventRepo,
			TaskLogs: logsRepo,
			Netboot:  netbootRepo,
			Opts: provision.ReaperOptions{
				Interval:       cfg.ReaperInterval,
				HeartbeatLimit: cfg.HeartbeatTimeout,
				IdempotencyTTL: cfg.IdempotencyTTL,
				TaskLogsTTL:    cfg.TaskLogsTTL,
				BootTreeDir:    filepath.Join(cfg.MediaDir, "netboot"),
			},
			Logger: logger,
		}
		go func() { _ = reaper.Run(ctx) }()

		// Webhook delivery: at-least-once, HMAC-signed (docs/09 M6).
		dispatcher := &webhook.Worker{
			Repo:   webhookRepo,
			Events: eventRepo,
			Crypto: crypto,
			Opts:   webhook.Options{Interval: 2 * time.Second},
			Logger: logger,
		}
		go func() { _ = dispatcher.Run(ctx) }()
	}

	// Runner facet (execution plane).
	if cfg.Mode.RunsRunner() {
		exec := &provision.Executor{
			Machines:        machineRepo,
			Credentials:     credRepo,
			Jobs:            jobRepo,
			Events:          eventRepo,
			Crypto:          crypto,
			BMC:             registry,
			Compat:          compatReg,
			Inband:          inband,
			Render:          renderReg,
			ExternalURL:     cfg.ExternalURL,
			MediaDir:        cfg.MediaDir,
			MediaWorkDir:    cfg.MediaWorkDir,
			MediaBaseURI:    cfg.MediaBaseURI,
			BootSettleDelay: cfg.BootSettleDelay,
			VerifyReadyWait: cfg.VerifyReadyWait,
			MediaUploader:   mediaUploader,
			RamdiskEnabled:  cfg.RamdiskEnabled,
			ProbeAlpineISO:  cfg.ProbeAlpineISO,
			ProbeStaticCIDR: cfg.ProbeStaticCIDR,
			ProbePrefix:     cfg.ProbePrefix,
			ProbeGateway:    cfg.ProbeGateway,
			ProbeWait:       cfg.ProbeWait,
			LayoutKeep:      cfg.LayoutRetention,
		}
		runner := provision.NewRunner(tq, jobRepo, eventRepo, exec, metrics, provision.RunnerOptions{
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
