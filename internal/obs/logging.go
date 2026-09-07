// Package obs centralizes observability: structured logging with the standard
// field set, Prometheus metrics, and OTel boundary instrumentation.
//
// Instrumentation is written in from day one (not retrofitted): a bare-metal
// install is a long async chain across api/runner/builder/BMC, so correlation
// (task_id end to end) must exist on the first day. See docs/10-tech-stack.md D6.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const loggerKey ctxKey = iota

// Standard field keys used across the whole codebase. Every log line that
// operates on a task/job/machine/request carries exactly these names.
const (
	FieldTaskID    = "task_id"
	FieldJobID     = "job_id"
	FieldMachineID = "machine_id"
	FieldRequestID = "request_id"
	FieldStage     = "stage"
	FieldRunnerID  = "runner_id"
	FieldQueue     = "queue"
	FieldBMCAddr   = "bmc_addr"
	FieldVendor    = "vendor"
	FieldOperation = "operation"
	FieldCode      = "code"
)

// NewLogger builds the process-wide slog.Logger.
func NewLogger(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var handler slog.Handler
	if strings.ToLower(format) == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// IntoContext attaches logger to ctx.
func IntoContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the logger carried by ctx, or the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// With returns a copy of ctx whose logger has attrs appended. Used to pin the
// standard fields (task_id, machine_id, …) at boundaries so every downstream
// log line inherits them without re-passing values.
func With(ctx context.Context, args ...any) context.Context {
	return IntoContext(ctx, FromContext(ctx).With(args...))
}
