package obs

import (
	"context"
	"log/slog"
	"time"
)

// TaskLogSink is the persistence side of the log dual-write. Implemented by
// *store.TaskLogRepo; kept as an interface so obs stays free of store.
type TaskLogSink interface {
	AppendTaskLog(ctx context.Context, taskID, level, stage, message string, attrs map[string]any)
}

// TaskLogTee is a slog.Handler wrapper implementing the log dual-write
// (docs/02-architecture.md §5.2): records that carry task_id are written to
// the sink in addition to the wrapped handler (local output). Lines without
// task_id (api request logs, runner lifecycle) only pass through — task_logs
// is the execution trail of one task, not a second event log.
//
// Writes are best-effort with a short detached deadline: log persistence
// must never slow down or fail the step being logged.
type TaskLogTee struct {
	inner slog.Handler
	sink  TaskLogSink
	// pinned accumulates WithAttrs state: slog does not put With-ed attrs on
	// the Record, so the tee keeps its own copy to see task_id pinned via
	// obs.With at the stage boundary (provision.Executor).
	pinned []slog.Attr
	group  string
}

// logWriteTimeout bounds each sink write, detached from the caller's ctx.
const logWriteTimeout = 3 * time.Second

// NewTaskLogTee wraps l so its task-scoped lines also persist via sink.
func NewTaskLogTee(l *slog.Logger, sink TaskLogSink) *slog.Logger {
	return slog.New(&TaskLogTee{inner: l.Handler(), sink: sink})
}

func (t *TaskLogTee) Enabled(ctx context.Context, l slog.Level) bool {
	return t.inner.Enabled(ctx, l)
}

func (t *TaskLogTee) Handle(ctx context.Context, r slog.Record) error {
	if t.sink != nil && r.Level >= slog.LevelDebug {
		taskID, stage, attrs := t.resolve(r)
		if taskID != "" {
			wctx, cancel := context.WithTimeout(context.Background(), logWriteTimeout)
			defer cancel()
			t.sink.AppendTaskLog(wctx, taskID, levelString(r.Level), stage, r.Message, attrs)
		}
	}
	return t.inner.Handle(ctx, r)
}

func (t *TaskLogTee) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &TaskLogTee{inner: t.inner.WithAttrs(attrs), sink: t.sink, group: t.group}
	next.pinned = append(next.pinned, t.pinned...)
	next.pinned = append(next.pinned, attrs...)
	return next
}

func (t *TaskLogTee) WithGroup(name string) slog.Handler {
	return &TaskLogTee{inner: t.inner.WithGroup(name), sink: t.sink,
		pinned: t.pinned, group: joinGroup(t.group, name)}
}

// resolve extracts task_id and stage, flattening the rest (pinned first, then
// the record's own attrs) into the persisted attrs payload.
func (t *TaskLogTee) resolve(r slog.Record) (taskID, stage string, attrs map[string]any) {
	rest := map[string]any{}
	var put func(prefix string, a slog.Attr)
	put = func(prefix string, a slog.Attr) {
		key := joinGroup(prefix, a.Key)
		if a.Value.Kind() == slog.KindGroup {
			for _, g := range a.Value.Group() {
				put(key, g)
			}
			return
		}
		switch key {
		case FieldTaskID:
			taskID = a.Value.String()
		case FieldStage:
			stage = a.Value.String()
		default:
			rest[key] = a.Value.Any()
		}
	}
	for _, a := range t.pinned {
		put(t.group, a)
	}
	r.Attrs(func(a slog.Attr) bool { put(t.group, a); return true })
	return taskID, stage, rest
}

func joinGroup(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func levelString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
