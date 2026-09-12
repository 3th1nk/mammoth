package provision

import (
	"context"
	"log/slog"
	"time"

	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// ReaperOptions size the control-plane reaper.
type ReaperOptions struct {
	Interval       time.Duration
	HeartbeatLimit time.Duration // running + silent this long → interrupted
	IdempotencyTTL time.Duration
	TaskLogsTTL    time.Duration // task_logs retention (90d default)
}

// Reaper converts lost heartbeats into retryable interrupted tasks. It is a
// control-plane component with exactly one sanctioned state write:
// interrupted (docs/08-data-model.md iron rule 1 & 3). A crashed runner thus
// leaves no permanently suspended task — the M0 acceptance hinges on this.
type Reaper struct {
	Jobs     *store.JobRepo
	Events   *store.EventRepo
	TaskLogs *store.TaskLogRepo
	Opts     ReaperOptions
	Logger   *slog.Logger
	Metrics  *obs.Metrics
}

func (r *Reaper) Run(ctx context.Context) error {
	interval := r.Opts.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	limit := r.Opts.HeartbeatLimit
	if limit <= 0 {
		limit = 60 * time.Second
	}
	ttl := r.Opts.IdempotencyTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	logsTTL := r.Opts.TaskLogsTTL
	if logsTTL <= 0 {
		logsTTL = 90 * 24 * time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var idemTick int
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		r.sweep(ctx, limit)
		// Replay-window cleanup once a minute is plenty.
		if idemTick++; idemTick%4 == 0 {
			if n, err := r.Jobs.ExpireIdempotencyKeys(ctx, ttl); err == nil && n > 0 {
				r.Logger.DebugContext(ctx, "expired idempotency keys", "count", n)
			}
			if r.TaskLogs != nil {
				if n, err := r.TaskLogs.ExpireTaskLogs(ctx, logsTTL); err == nil && n > 0 {
					r.Logger.DebugContext(ctx, "expired task logs", "count", n)
				}
			}
		}
	}
}

func (r *Reaper) sweep(ctx context.Context, limit time.Duration) {
	ids, err := r.Jobs.InterruptStaleTasks(ctx, limit)
	if err != nil {
		r.Logger.ErrorContext(ctx, "reaper sweep failed", "err", err.Error())
		return
	}
	for _, id := range ids {
		task, err := r.Jobs.GetTask(ctx, id)
		if err != nil {
			continue
		}
		r.Events.Append(ctx, "task", id, "task.interrupted", map[string]any{
			"machine_id": task.MachineID, "job_id": task.JobID,
		})
		_ = r.Jobs.RecomputeJob(ctx, task.JobID)
		r.Logger.WarnContext(ctx, "task interrupted by reaper",
			obs.FieldTaskID, id, obs.FieldMachineID, task.MachineID)
	}
	if len(ids) > 0 {
		r.Logger.WarnContext(ctx, "reaper interrupted tasks", "count", len(ids))
	}
}
