// Package config loads Mammoth runtime configuration from environment
// variables (12-factor style; a mounted config file is unnecessary for the
// current surface). All durations accept Go duration strings (e.g. 10s, 1m).
package config

import (
	"fmt"
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

	// OTELExporterEndpoint enables OTLP trace export when non-empty.
	// Without it, tracing stays at the API boundary instrumentation level
	// with a no-op exporter (zero overhead, zero dependencies).
	OTELExporterEndpoint string

	// MetricsAddr serves /metrics; empty = serve on HTTPAddr alongside the API.
	MetricsAddr string
}

// FromEnv builds a Config from the process environment with the MAMMOTH_ prefix.
func FromEnv() (Config, error) {
	c := Config{
		Mode:                  Mode(getenv("MAMMOTH_MODE", string(ModeAll))),
		DatabaseURL:           getenv("MAMMOTH_DATABASE_URL", ""),
		HTTPAddr:              getenv("MAMMOTH_HTTP_ADDR", ":8080"),
		APIToken:              os.Getenv("MAMMOTH_API_TOKEN"),
		MasterKey:             os.Getenv("MAMMOTH_MASTER_KEY"),
		LogLevel:              getenv("MAMMOTH_LOG_LEVEL", "info"),
		LogFormat:             getenv("MAMMOTH_LOG_FORMAT", "json"),
		RunnerConcurrency:     getenvInt("MAMMOTH_RUNNER_CONCURRENCY", 10),
		HeartbeatInterval:     getenvDuration("MAMMOTH_HEARTBEAT_INTERVAL", 10*time.Second),
		VisibilityTimeout:     getenvDuration("MAMMOTH_VISIBILITY_TIMEOUT", 30*time.Second),
		HeartbeatTimeout:      getenvDuration("MAMMOTH_HEARTBEAT_TIMEOUT", 60*time.Second),
		ReaperInterval:        getenvDuration("MAMMOTH_REAPER_INTERVAL", 15*time.Second),
		QueueMaxReceiveCount:  getenvInt("MAMMOTH_QUEUE_MAX_RECEIVE_COUNT", 5),
		QueueRetryBackoff:     getenvDuration("MAMMOTH_QUEUE_RETRY_BACKOFF", 5*time.Second),
		QueuePollInterval:     getenvDuration("MAMMOTH_QUEUE_POLL_INTERVAL", 250*time.Millisecond),
		RunnerMaxTaskAttempts: getenvInt("MAMMOTH_RUNNER_MAX_TASK_ATTEMPTS", 5),
		BMCTimeout:            getenvDuration("MAMMOTH_BMC_TIMEOUT", 30*time.Second),
		BMCTLSInsecure:        getenvBool("MAMMOTH_BMC_TLS_INSECURE", false),
		IPMIInterface:         getenv("MAMMOTH_IPMI_INTERFACE", "lanplus"),
		IdempotencyTTL:        getenvDuration("MAMMOTH_IDEMPOTENCY_TTL", 24*time.Hour),
		TaskStatusInterval:    getenvDuration("MAMMOTH_TASK_STATUS_INTERVAL", 2*time.Second),
		OTELExporterEndpoint:  os.Getenv("MAMMOTH_OTEL_EXPORTER_ENDPOINT"),
		MetricsAddr:           os.Getenv("MAMMOTH_METRICS_ADDR"),
	}
	if !c.Mode.Valid() {
		return c, fmt.Errorf("config: invalid MAMMOTH_MODE %q (want all|api|runner|builder|prober)", c.Mode)
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("config: MAMMOTH_DATABASE_URL is required")
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
