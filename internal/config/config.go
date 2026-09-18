// Package config loads Mammoth runtime configuration from environment
// variables (12-factor style), optionally seeded from a dotenv file
// (MAMMOTH_ENV_FILE / serve --env-file; existing environment wins).
// All durations accept Go duration strings (e.g. 10s, 1m). Invalid values
// are configuration errors and fail startup — silent defaults would hide
// typos in exactly the knobs an operator is deliberately tuning.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode is a runtime facet of the single binary.
type Mode string

const (
	ModeAll     Mode = "all"
	ModeAPI     Mode = "api"
	ModeRunner  Mode = "runner"
	ModeBuilder Mode = "builder"
	ModeProber  Mode = "prober"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeAll, ModeAPI, ModeRunner, ModeBuilder, ModeProber:
		return true
	}
	return false
}

func (m Mode) RunsAPI() bool    { return m == ModeAll || m == ModeAPI }
func (m Mode) RunsRunner() bool { return m == ModeAll || m == ModeRunner }
func (m Mode) RunsReaper() bool { return m == ModeAll || m == ModeAPI }

// RunsNetboot reports whether the facet hosts the PXE network boot services
// (proxyDHCP + TFTP): they must run beside the machine-face HTTP endpoints
// (script URLs point at this host), and a provisioning L2 has exactly one
// responder — the same "one per deployment" assumption as the API facet.
func (m Mode) RunsNetboot() bool { return m == ModeAll || m == ModeAPI }

// Config is the full runtime configuration. Every facet reads the same struct;
// modes only decide which components are started.
type Config struct {
	// Mode selects the facets to run (serve --mode).
	Mode Mode

	// PostgreSQL DSN, the only runtime-strong external dependency.
	DatabaseURL string

	HTTPAddr string
	// APIToken is the static bearer token. When empty, the API refuses
	// bearer-authenticated requests unless DevAllowAnonymous is set (never
	// for production; default false).
	APIToken string

	// MasterKey is the base64-encoded 32-byte key used for credential
	// encryption at rest. Required when storing credentials.
	MasterKey string

	LogLevel  string // debug | info | warn | error
	LogFormat string // json | text

	RunnerConcurrency     int
	HeartbeatInterval     time.Duration
	VisibilityTimeout     time.Duration
	HeartbeatTimeout      time.Duration // reaper threshold for interrupted
	ReaperInterval        time.Duration
	QueueMaxReceiveCount  int           // dead-letter threshold per message
	QueueRetryBackoff     time.Duration // base nack delay; exponential per delivery count
	QueuePollInterval     time.Duration
	RunnerMaxTaskAttempts int // per-task total attempt ceiling before failed

	BMCTimeout         time.Duration
	BMCTLSInsecure     bool
	IPMIInterface      string // lanplus (default) | lan
	IdempotencyTTL     time.Duration
	TaskStatusInterval time.Duration // terminal-transition sweep for job summary
	TaskLogsTTL        time.Duration // task_logs retention (reaper-expired; 90d default)
	LayoutRetention    int           // snapshots kept per machine (docs/08: default 10)
	InbandTimeout      time.Duration // whole inband_ssh collection bound

	// Ramdisk probe (docs/05-inventory.md §4; alpine virtual-media carrier).
	RamdiskEnabled     bool
	ProbeAlpineISO     string // alpine standard ISO (path/URL) the probe medium is built from
	ProbeAlpineNetboot string // alpine NETBOOT tarball (path/URL) for the probe's PXE carrier (network drivers included)
	PXEDINetboot       string // debian d-i netboot.tar.gz (path/URL) — the PXE carrier for debian12 (the ISO's initrd is the cdrom flavour)
	PXEDIUdebsDir      string // staged netboot udeb archive subset (scripts/fetch-di-udebs.sh shape) filling the netinst ISO's pruned pool
	ProbeStaticCIDR    string // DHCP fallback for machines without ssh.address
	ProbePrefix        int    // prefix length for a bare ssh.address fallback CIDR (default 24)
	ProbeGateway       string // fallback default route (cross-subnet report targets)
	ProbeWait          time.Duration

	MediaDir        string // local media repository (boot ISOs)
	MediaWorkDir    string // scratch dir for media builds (default: beside the output)
	MediaBaseURI    string // BMC-reachable media base URI (nfs://, cifs://, ftp:// — firmware decides)
	BootSettleDelay time.Duration

	VerifyReadyWait time.Duration // verify_ready poll budget for the new system after the completion report

	NFSExportEnabled   bool   // built-in read-only NFSv3 export of MediaDir (default true)
	NFSExportPort      int    // port for the built-in export (default 2049)
	MediaRelayAddr     string // media relay SSH endpoint (host[:port]); empty = no relay
	MediaRelayUser     string
	MediaRelayPassword string
	MediaRelayDir      string // remote export directory the BMC mounts from

	// Vendor compatibility matrix override directory (docs/compat/README.md).
	CompatDir string
	// BiosConfirmRequired gates the set_bios_attributes action's two-stage
	// confirmation (docs/07-bmc.md §6): true (default) rejects requests
	// without the explicit confirm flag at submit; automation deployments
	// may set MAMMOTH_BIOS_CONFIRM=optional to skip the flag (the runner's
	// live-table validation always stays on).
	BiosConfirmRequired bool
	// EraseConfirmRequired gates the erase_drives action's two-stage
	// confirmation (docs/07-bmc.md §6.2) the same way, via
	// MAMMOTH_ERASE_CONFIRM — the most destructive action in the surface.
	EraseConfirmRequired bool

	// DevFakeBMCDelay slows the fake BMC/inband drivers to exercise
	// heartbeat/lease/reaper paths (acceptance flow; a test knob, not a
	// production one).
	DevFakeBMCDelay time.Duration

	// OTELExporterEndpoint enables OTLP trace export when non-empty.
	// Without it, tracing stays at the API boundary instrumentation level
	// with a no-op exporter (zero overhead, zero dependencies).
	OTELExporterEndpoint string

	// MetricsAddr serves /metrics; empty = serve on HTTPAddr alongside the API.
	MetricsAddr string

	// ExternalURL is the base address machines reach for answer files
	// (docs/06-install-pipeline.md §2.1: inst.ks target).
	ExternalURL string

	// PXE network boot services (docs/06-install-pipeline.md §3.3, M7).
	// PXEEnabled turns on proxyDHCP + TFTP (MAMMOTH_PXE_ENABLED); binding
	// privileged ports and sharing the provisioning L2 with the site DHCP
	// are deployment concerns — see docs/operations.md.
	PXEEnabled    bool
	PXENextServer string // mammoth's IPv4 on the provisioning L2 (DHCP next-server); derived from ExternalURL when it is an IP literal
	// PXEMode selects who owns the UDP side of PXE (MAMMOTH_PXE_MODE):
	// builtin (default) — mammoth's proxyDHCP+TFTP; external — the escape
	// hatch for shapes where mammoth cannot be the PXE service (site
	// dnsmasq owns DHCP+TFTP, mammoth serves HTTP only; the static kit is
	// exported to MediaDir/netboot/external-tftp at startup,
	// docs/operations.md §pxe-external).
	PXEMode      string
	PXEDHCPPort  int // proxyDHCP listen (default 67)
	PXETFTPPort  int // NBP transfer (default 69)
	PXEProxyPort int // PXE boot-server discovery (default 4011)
	// PXESyslogPort is the installer-log sink port (MAMMOTH_PXE_SYSLOG_PORT,
	// default 514): d-i forwards its ramfs syslog here via the syslog= kernel
	// argument, and lines that resolve to an armed task land in task_logs —
	// the installer environment dies with the ramfs, a post-mortem otherwise
	// has nothing (related-work §2). A busy port degrades to a warning: the
	// sink is a diagnostic, never a lifeline.
	PXESyslogPort int
	// PXEDHCPPool (optional) turns the responder into a full DHCP for PXE
	// clients — for provisioning L2s WITHOUT a site DHCP, where a boot ROM
	// otherwise never gets an IP lease. Two syntaxes: dash range
	// ("10.0.0.10-10.0.0.50" or "10.0.0.10-50") or comma list of individual
	// addresses ("10.0.0.10,10.0.0.20"). The installer kernel's DHCP is
	// served too; ordinary L2 hosts are not. Leave empty when a site DHCP
	// exists.
	PXEDHCPPool   string
	PXEDHCPRouter string // optional lease router; defaults to PXENextServer
	// PXEBroadcastAddr (optional) replaces the limited broadcast
	// (255.255.255.255) in PXE replies with a directed broadcast — macOS
	// routing sends 255.255.255.255 via the default interface, which never
	// reaches guests behind a local vmnet bridge (qemu verification setup).
	// Linux deployments need no override.
	PXEBroadcastAddr string
	// PXEEnroll turns on the zero-registration entry (MAMMOTH_PXE_ENROLL,
	// docs/09-roadmap.md): unknown MACs are offered the shared enrollment
	// payload — an alpine probe environment that scans /sys and reports
	// into pending_machines. Needs the alpine NETBOOT tarball
	// (MAMMOTH_PROBE_ALPINE_NETBOOT); usually enabled alongside PXEEnabled.
	PXEEnroll bool
	// PXEEnrollToken is the enrollment endpoint's credential
	// (MAMMOTH_PXE_ENROLL_TOKEN) — required when PXEEnroll is on, a mismatch
	// is indistinguishable from enrollment being off.
	PXEEnrollToken string
	// BootStrategyDefault is the carrier used when a job spec does not name
	// one (MAMMOTH_BOOT_STRATEGY: virtual_media | pxe). pxe additionally
	// requires PXEEnabled on the deployment.
	BootStrategyDefault string
}

// Load reads MAMMOTH_ENV_FILE (when set), then builds the Config from the
// process environment with the MAMMOTH_ prefix. File entries seed the
// environment; variables already set in the process environment win.
func Load() (Config, error) {
	if path := getenv("MAMMOTH_ENV_FILE", ""); path != "" {
		if err := LoadEnvFile(path); err != nil {
			return Config{}, err
		}
	}
	return FromEnv()
}

// FromEnv builds a Config from the process environment. Invalid values are
// errors (fail fast) rather than silent defaults.
func FromEnv() (Config, error) {
	c := Config{
		Mode:                  Mode(getenv("MAMMOTH_MODE", string(ModeAll))),
		DatabaseURL:           getenv("MAMMOTH_DATABASE_URL", ""),
		HTTPAddr:              getenv("MAMMOTH_HTTP_ADDR", ":8080"),
		APIToken:              os.Getenv("MAMMOTH_API_TOKEN"),
		MasterKey:             os.Getenv("MAMMOTH_MASTER_KEY"),
		LogLevel:              getenv("MAMMOTH_LOG_LEVEL", "info"),
		LogFormat:             getenv("MAMMOTH_LOG_FORMAT", "json"),
		RunnerConcurrency:     10,
		BiosConfirmRequired:   true,
		HeartbeatInterval:     10 * time.Second,
		VisibilityTimeout:     30 * time.Second,
		HeartbeatTimeout:      60 * time.Second,
		ReaperInterval:        15 * time.Second,
		QueueMaxReceiveCount:  5,
		QueueRetryBackoff:     5 * time.Second,
		QueuePollInterval:     250 * time.Millisecond,
		RunnerMaxTaskAttempts: 5,
		BMCTimeout:            30 * time.Second,
		IPMIInterface:         "lanplus",
		IdempotencyTTL:        24 * time.Hour,
		TaskStatusInterval:    2 * time.Second,
		TaskLogsTTL:           90 * 24 * time.Hour,
		LayoutRetention:       10,
		InbandTimeout:         20 * time.Second,
		ProbePrefix:           24,
		ProbeWait:             10 * time.Minute,
		MediaDir:              "data/media",
		NFSExportEnabled:      true,
		NFSExportPort:         2049,
		BootSettleDelay:       0,
		VerifyReadyWait:       10 * time.Minute,
		ExternalURL:           "http://127.0.0.1:8080",
		PXEDHCPPort:           67,
		PXETFTPPort:           69,
		PXEProxyPort:          4011,
	}

	var errs []error
	setString(&c.MediaWorkDir, "MAMMOTH_MEDIA_WORKDIR", &errs)
	setString(&c.MediaBaseURI, "MAMMOTH_MEDIA_BASE_URI", &errs)
	setString(&c.MediaRelayAddr, "MAMMOTH_MEDIA_RELAY_ADDR", &errs)
	setString(&c.MediaRelayUser, "MAMMOTH_MEDIA_RELAY_USER", &errs)
	setString(&c.MediaRelayPassword, "MAMMOTH_MEDIA_RELAY_PASSWORD", &errs)
	setString(&c.MediaRelayDir, "MAMMOTH_MEDIA_RELAY_DIR", &errs)
	setString(&c.OTELExporterEndpoint, "MAMMOTH_OTEL_EXPORTER_ENDPOINT", &errs)
	setString(&c.MetricsAddr, "MAMMOTH_METRICS_ADDR", &errs)
	setString(&c.CompatDir, "MAMMOTH_COMPAT_DIR", &errs)

	applyString(&c.HTTPAddr, "MAMMOTH_HTTP_ADDR", &errs)
	applyString(&c.LogLevel, "MAMMOTH_LOG_LEVEL", &errs)
	applyString(&c.LogFormat, "MAMMOTH_LOG_FORMAT", &errs)
	applyString(&c.IPMIInterface, "MAMMOTH_IPMI_INTERFACE", &errs)
	applyString(&c.ProbeAlpineISO, "MAMMOTH_PROBE_ALPINE_ISO", &errs)
	applyString(&c.ProbeAlpineNetboot, "MAMMOTH_PROBE_ALPINE_NETBOOT", &errs)
	applyString(&c.PXEDINetboot, "MAMMOTH_PXE_DI_NETBOOT", &errs)
	applyString(&c.PXEDIUdebsDir, "MAMMOTH_PXE_DI_UDEBS_DIR", &errs)
	applyString(&c.ProbeStaticCIDR, "MAMMOTH_PROBE_STATIC_CIDR", &errs)
	applyString(&c.ProbeGateway, "MAMMOTH_PROBE_GATEWAY", &errs)
	applyString(&c.MediaDir, "MAMMOTH_MEDIA_DIR", &errs)
	applyString(&c.ExternalURL, "MAMMOTH_EXTERNAL_URL", &errs)

	applyInt(&c.RunnerConcurrency, "MAMMOTH_RUNNER_CONCURRENCY", &errs)
	applyInt(&c.QueueMaxReceiveCount, "MAMMOTH_QUEUE_MAX_RECEIVE_COUNT", &errs)
	applyInt(&c.RunnerMaxTaskAttempts, "MAMMOTH_RUNNER_MAX_TASK_ATTEMPTS", &errs)
	applyInt(&c.LayoutRetention, "MAMMOTH_LAYOUT_RETENTION", &errs)
	applyInt(&c.NFSExportPort, "MAMMOTH_NFS_EXPORT_PORT", &errs)
	applyInt(&c.ProbePrefix, "MAMMOTH_PROBE_PREFIX", &errs)
	applyInt(&c.PXEDHCPPort, "MAMMOTH_PXE_DHCP_PORT", &errs)
	applyInt(&c.PXETFTPPort, "MAMMOTH_PXE_TFTP_PORT", &errs)
	applyInt(&c.PXEProxyPort, "MAMMOTH_PXE_PROXY_PORT", &errs)
	applyInt(&c.PXESyslogPort, "MAMMOTH_PXE_SYSLOG_PORT", &errs)

	applyDuration(&c.HeartbeatInterval, "MAMMOTH_HEARTBEAT_INTERVAL", &errs)
	applyDuration(&c.VisibilityTimeout, "MAMMOTH_VISIBILITY_TIMEOUT", &errs)
	applyDuration(&c.HeartbeatTimeout, "MAMMOTH_HEARTBEAT_TIMEOUT", &errs)
	applyDuration(&c.ReaperInterval, "MAMMOTH_REAPER_INTERVAL", &errs)
	applyDuration(&c.QueueRetryBackoff, "MAMMOTH_QUEUE_RETRY_BACKOFF", &errs)
	applyDuration(&c.QueuePollInterval, "MAMMOTH_QUEUE_POLL_INTERVAL", &errs)
	applyDuration(&c.BMCTimeout, "MAMMOTH_BMC_TIMEOUT", &errs)
	applyDuration(&c.IdempotencyTTL, "MAMMOTH_IDEMPOTENCY_TTL", &errs)
	applyDuration(&c.TaskStatusInterval, "MAMMOTH_TASK_STATUS_INTERVAL", &errs)
	applyDuration(&c.TaskLogsTTL, "MAMMOTH_TASK_LOGS_TTL", &errs)
	applyDuration(&c.InbandTimeout, "MAMMOTH_INBAND_TIMEOUT", &errs)
	applyDuration(&c.ProbeWait, "MAMMOTH_PROBE_WAIT", &errs)
	applyDuration(&c.BootSettleDelay, "MAMMOTH_BOOT_SETTLE_DELAY", &errs)
	applyDuration(&c.VerifyReadyWait, "MAMMOTH_VERIFY_READY_WAIT", &errs)
	applyDuration(&c.DevFakeBMCDelay, "MAMMOTH_FAKE_BMC_DELAY", &errs)

	applyBool(&c.BMCTLSInsecure, "MAMMOTH_BMC_TLS_INSECURE", &errs)
	// MAMMOTH_BIOS_CONFIRM=optional disables the submit-side confirm flag
	// requirement (two-stage contract, docs/07-bmc.md §6); anything other
	// than "optional"/"required" is a startup error.
	switch getenv("MAMMOTH_BIOS_CONFIRM", "required") {
	case "required":
		c.BiosConfirmRequired = true
	case "optional":
		c.BiosConfirmRequired = false
	default:
		errs = append(errs, fmt.Errorf("MAMMOTH_BIOS_CONFIRM must be required|optional"))
	}
	// MAMMOTH_ERASE_CONFIRM gates erase_drives the same way (docs/07-bmc.md
	// §6.2); required (default) keeps the submit-side confirm flag.
	switch getenv("MAMMOTH_ERASE_CONFIRM", "required") {
	case "required":
		c.EraseConfirmRequired = true
	case "optional":
		c.EraseConfirmRequired = false
	default:
		errs = append(errs, fmt.Errorf("MAMMOTH_ERASE_CONFIRM must be required|optional"))
	}
	applyBool(&c.RamdiskEnabled, "MAMMOTH_RAMDISK_ENABLED", &errs)
	applyBool(&c.NFSExportEnabled, "MAMMOTH_NFS_EXPORT", &errs)
	applyBool(&c.PXEEnabled, "MAMMOTH_PXE_ENABLED", &errs)
	applyString(&c.PXEMode, "MAMMOTH_PXE_MODE", &errs)
	applyString(&c.PXENextServer, "MAMMOTH_PXE_NEXT_SERVER", &errs)
	applyString(&c.PXEDHCPPool, "MAMMOTH_PXE_DHCP_POOL", &errs)
	applyString(&c.PXEDHCPRouter, "MAMMOTH_PXE_DHCP_ROUTER", &errs)
	applyString(&c.PXEBroadcastAddr, "MAMMOTH_PXE_BROADCAST_ADDR", &errs)
	applyBool(&c.PXEEnroll, "MAMMOTH_PXE_ENROLL", &errs)
	applyString(&c.PXEEnrollToken, "MAMMOTH_PXE_ENROLL_TOKEN", &errs)
	applyString(&c.BootStrategyDefault, "MAMMOTH_BOOT_STRATEGY", &errs)

	if err := errors.Join(errs...); err != nil {
		return c, err
	}
	if !c.Mode.Valid() {
		return c, fmt.Errorf("config: invalid MAMMOTH_MODE %q (want all|api|runner|builder|prober)", c.Mode)
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("config: MAMMOTH_DATABASE_URL is required")
	}
	switch c.PXEMode {
	case "":
		c.PXEMode = "builtin"
	case "builtin", "external":
	default:
		return c, fmt.Errorf("config: invalid MAMMOTH_PXE_MODE %q (want builtin|external)", c.PXEMode)
	}
	if c.PXEMode == "external" && c.PXEDHCPPool != "" {
		return c, fmt.Errorf("config: MAMMOTH_PXE_MODE=external owns no DHCP — unset MAMMOTH_PXE_DHCP_POOL (the site dnsmasq serves leases)")
	}
	// PXE needs an IPv4 next-server for the DHCP replies. Derive it from
	// ExternalURL when that is an IP literal; otherwise require it — a
	// hostname cannot go into siaddr/option 66, and failing at configure
	// time beats failing on the first PXE boot. External mode never speaks
	// DHCP (the site server fills next-server), so no requirement there.
	if c.PXEEnabled && c.PXEMode == "builtin" && c.PXENextServer == "" {
		if host, _, splitErr := net.SplitHostPort(strings.TrimSuffix(c.ExternalURL, "/")); splitErr == nil {
			c.PXENextServer = host
		} else if u, uerr := url.Parse(c.ExternalURL); uerr == nil {
			c.PXENextServer = u.Hostname()
		}
		if net.ParseIP(c.PXENextServer) == nil || net.ParseIP(c.PXENextServer).To4() == nil {
			return c, fmt.Errorf("config: MAMMOTH_PXE_ENABLED requires MAMMOTH_PXE_NEXT_SERVER (an IPv4 address on the provisioning L2); ExternalURL host %q is not an IPv4 literal", c.PXENextServer)
		}
	}
	switch c.BootStrategyDefault {
	case "":
		c.BootStrategyDefault = "virtual_media"
	case "virtual_media":
	case "pxe":
		if !c.PXEEnabled {
			return c, fmt.Errorf("config: MAMMOTH_BOOT_STRATEGY=pxe requires MAMMOTH_PXE_ENABLED (the netboot service must run somewhere)")
		}
	default:
		return c, fmt.Errorf("config: MAMMOTH_BOOT_STRATEGY %q is not one of virtual_media|pxe", c.BootStrategyDefault)
	}
	// The enrollment payload without a token would let anyone on the L2
	// inject pending sightings; requiring it at configure time keeps the
	// feature explicit (a missing token is indistinguishable from enrollment
	// being off — and that ambiguity is worth avoiding).
	if c.PXEEnroll && c.PXEEnrollToken == "" {
		return c, fmt.Errorf("config: MAMMOTH_PXE_ENROLL requires MAMMOTH_PXE_ENROLL_TOKEN (the enrollment endpoint's credential)")
	}
	return c, nil
}

// setString reads an optional variable (empty = unset semantics).
func setString(dst *string, key string, errs *[]error) {
	*dst = os.Getenv(key)
}

// applyString overrides dst when the variable is set to a non-empty value.
func applyString(dst *string, key string, errs *[]error) {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		*dst = v
	}
}

func applyInt(dst *int, key string, errs *[]error) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("config: %s=%q is not an integer", key, v))
			return
		}
		*dst = n
	}
}

func applyBool(dst *bool, key string, errs *[]error) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("config: %s=%q is not a boolean", key, v))
			return
		}
		*dst = b
	}
}

func applyDuration(dst *time.Duration, key string, errs *[]error) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("config: %s=%q is not a duration (want e.g. 10s, 1m)", key, v))
			return
		}
		*dst = d
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
