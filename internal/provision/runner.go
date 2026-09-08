package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
)

// Options size the runner.
type RunnerOptions struct {
	Concurrency     int
	PollInterval    time.Duration
	Visibility      time.Duration // dequeue lease window
	HeartbeatEvery  time.Duration
	MaxTaskAttempts int
}

// Runner claims task messages and drives them through their flow. Every task
// carries a heartbeat; losing it (crash, network partition) makes the task
// reapable — nothing stays suspended forever (docs/02-architecture.md §1).
type Runner struct {
	ID      string
	Queue   queue.TaskQueue
	Jobs    *store.JobRepo
	Events  *store.EventRepo
	Exec    *Executor
	Metrics *obs.Metrics
	Opts    RunnerOptions
	Logger  *slog.Logger
}

func NewRunner(q queue.TaskQueue, jobs *store.JobRepo, events *store.EventRepo, exec *Executor, m *obs.Metrics, o RunnerOptions, logger *slog.Logger) *Runner {
	if o.Concurrency <= 0 {
		o.Concurrency = 10
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 250 * time.Millisecond
	}
	if o.HeartbeatEvery <= 0 {
		o.HeartbeatEvery = 10 * time.Second
	}
	if o.MaxTaskAttempts <= 0 {
		o.MaxTaskAttempts = 5
	}
	return &Runner{
		ID:      "runner-" + uuid.NewString()[:8],
		Queue:   q,
		Jobs:    jobs,
		Events:  events,
		Exec:    exec,
		Metrics: m,
		Opts:    o,
		Logger:  logger,
	}
}

// Run blocks until ctx is canceled, spawning Concurrency workers.
func (r *Runner) Run(ctx context.Context) error {
	r.Logger.InfoContext(ctx, "runner starting",
		obs.FieldRunnerID, r.ID, "concurrency", r.Opts.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < r.Opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.worker(ctx, i)
		}()
	}
	// Depth gauge refresher.
	go r.reportDepth(ctx)
	wg.Wait()
	r.Logger.InfoContext(ctx, "runner stopped", obs.FieldRunnerID, r.ID)
	return nil
}

func (r *Runner) worker(ctx context.Context, n int) {
	log := r.Logger.With(obs.FieldRunnerID, r.ID, "worker", n)
	for {
		if ctx.Err() != nil {
			return
		}
		receipt, err := r.Queue.Dequeue(ctx, QueueTasks, r.Opts.Visibility)
		if err != nil {
			if !errors.Is(err, queue.ErrEmpty) && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				log.WarnContext(ctx, "dequeue failed", "err", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.Opts.PollInterval):
			}
			continue
		}
		r.handle(ctx, receipt, log)
	}
}

// handle executes one claimed message end to end: claim → stages → ack/nack.
func (r *Runner) handle(ctx context.Context, receipt queue.Receipt, log *slog.Logger) {
	msg, err := UnmarshalMessage(receipt.Payload)
	if err != nil {
		// Poison envelope: ack to drop; dead-letter is unnecessary — the
		// message can never become work.
		log.ErrorContext(ctx, "dropping unparseable message", "err", err.Error())
		_ = r.Queue.Ack(ctx, receipt)
		return
	}
	ctx = obs.ExtractTraceContext(ctx, msg.Trace)
	ctx, span := obs.Tracer().Start(ctx, "queue.consume")
	defer span.End()
	ctx = obs.With(ctx, obs.FieldTaskID, msg.TaskID)

	task, err := r.Jobs.GetTask(ctx, msg.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		log.WarnContext(ctx, "message for unknown task, dropping")
		_ = r.Queue.Ack(ctx, receipt)
		return
	} else if err != nil {
		log.ErrorContext(ctx, "load task failed", "err", err.Error())
		_ = r.Queue.Nack(ctx, receipt, err)
		return
	}

	// Claim: only a pending task accepts a new owner. Interrupted tasks are
	// retried explicitly via the API (a fresh message), so an old redelivery
	// of an interrupted task is stale and gets acked away.
	claimed, err := r.Jobs.ClaimTask(ctx, task.ID, r.ID, r.Opts.Visibility)
	if err != nil {
		log.ErrorContext(ctx, "claim failed", "err", err.Error())
		_ = r.Queue.Nack(ctx, receipt, err)
		return
	}
	if !claimed {
		log.InfoContext(ctx, "stale message for non-pending task, acking",
			"task_state", task.State)
		_ = r.Queue.Ack(ctx, receipt)
		return
	}
	if r.Metrics != nil {
		r.Metrics.QueueMsgs.WithLabelValues(receipt.Queue, "claim").Inc()
	}
	ctx = obs.With(ctx, obs.FieldMachineID, task.MachineID, obs.FieldJobID, task.JobID)
	log.InfoContext(ctx, "task claimed")

	// Reload to see claimed state; fetch the job for flow context.
	task, err = r.Jobs.GetTask(ctx, task.ID)
	if err != nil {
		_ = r.Queue.Nack(ctx, receipt, err)
		return
	}
	job, err := r.Jobs.GetJob(ctx, task.JobID)
	if err != nil {
		_ = r.Queue.Nack(ctx, receipt, err)
		return
	}
	_ = r.Jobs.RecomputeJob(ctx, job.ID)

	// Heartbeat keeps ownership alive and honors cooperative cancel.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go r.heartbeat(hbCtx, receipt, task.ID, r.ID, log)

	// Execute stages from the persisted index (retry = re-enter here).
	stages := StageNames(task.FlowName)
	var execErr error
	for seq := task.StageIndex; seq < len(stages); seq++ {
		if ctx.Err() != nil {
			execErr = ctx.Err()
			break
		}
		if cancelReq, _ := r.Jobs.IsCancelRequested(ctx, task.ID); cancelReq {
			execErr = ErrCanceled
			break
		}
		if err := r.Jobs.StartStage(ctx, task.ID, seq); err != nil {
			log.ErrorContext(ctx, "start stage failed", "err", err.Error())
		}
		err := r.Exec.ExecuteStage(ctx, task, job, seq)
		if err == nil {
			if err := r.Jobs.AdvanceStage(ctx, task.ID, seq); err != nil {
				execErr = fmt.Errorf("advance stage: %w", err)
				break
			}
			r.event(ctx, task, "task.stage_changed", map[string]any{
				"stage": stages[seq], "seq": seq,
			})
			continue
		}
		_ = r.Jobs.FailStage(ctx, task.ID, seq)
		execErr = err
		break
	}

	hbCancel()
	r.finish(ctx, receipt, task, job, execErr, log)
}

// finish classifies the outcome: success, cooperative cancel, retryable
// failure (nack → redelivery with backoff), or terminal failure.
func (r *Runner) finish(ctx context.Context, receipt queue.Receipt, task *store.Task, job *store.Job, execErr error, log *slog.Logger) {
	switch {
	case execErr == nil:
		if err := r.Jobs.CompleteTask(ctx, task.ID); err != nil {
			log.ErrorContext(ctx, "complete task failed", "err", err.Error())
		}
		r.event(ctx, task, "task.state_changed", map[string]any{"state": "succeeded"})
		r.observe(ctx, job, "succeeded")
		mustAck(ctx, r.Queue, receipt, log)
		r.afterStateChange(ctx, job, nil)
		log.InfoContext(ctx, "task succeeded")

	case IsCanceled(execErr):
		// Compensate side effects, then mark canceled (runner-owned write).
		r.Exec.Compensate(ctx, task, job)
		if err := r.Jobs.MarkCanceled(ctx, task.ID); err != nil {
			log.ErrorContext(ctx, "cancel task failed", "err", err.Error())
		}
		r.event(ctx, task, "task.state_changed", map[string]any{"state": "canceled"})
		r.observe(ctx, job, "canceled")
		mustAck(ctx, r.Queue, receipt, log)
		r.afterStateChange(ctx, job, nil)
		log.InfoContext(ctx, "task canceled")

	case classified(execErr).Retryable && task.StageAttempt+1 < r.Opts.MaxTaskAttempts:
		ei := classified(execErr)
		if err := r.Jobs.BumpAttempt(ctx, task.ID, ei); err != nil {
			// Could not hand back to pending; treat as terminal to avoid
			// unbounded duplicate execution.
			log.ErrorContext(ctx, "bump attempt failed, failing task", "err", err.Error())
			_ = r.Jobs.FailTask(ctx, task.ID, ei)
			r.observe(ctx, job, "failed")
			mustAck(ctx, r.Queue, receipt, log)
			r.afterStateChange(ctx, job, &ei)
			return
		}
		r.observe(ctx, job, "retry")
		if err := r.Queue.Nack(ctx, receipt, execErr); err != nil {
			log.ErrorContext(ctx, "nack failed", "err", err.Error())
		} else if r.Metrics != nil {
			r.Metrics.QueueMsgs.WithLabelValues(receipt.Queue, "nack").Inc()
		}
		log.WarnContext(ctx, "task failed, will retry",
			obs.FieldCode, ei.Code, "attempt", task.StageAttempt+1)

	default:
		ei := classified(execErr)
		if err := r.Jobs.FailTask(ctx, task.ID, ei); err != nil {
			log.ErrorContext(ctx, "fail task failed", "err", err.Error())
		}
		r.event(ctx, task, "task.state_changed", map[string]any{"state": "failed", "code": ei.Code})
		r.observe(ctx, job, "failed")
		mustAck(ctx, r.Queue, receipt, log)
		r.afterStateChange(ctx, job, &ei)
		log.ErrorContext(ctx, "task failed terminally", obs.FieldCode, ei.Code, "err", ei.Message)
	}
	_ = r.Jobs.RecomputeJob(ctx, job.ID)
}

// afterStateChange applies batch policy when a task reaches a terminal state.
func (r *Runner) afterStateChange(ctx context.Context, job *store.Job, ei *store.ErrorInfo) {
	if ei == nil || !job.Policy.AbortBatch() {
		return
	}
	// abort_batch: cancel everything not finished yet.
	if _, err := r.Jobs.CancelJobTasks(ctx, job.ID); err != nil {
		obs.FromContext(ctx).WarnContext(ctx, "abort_batch cancel failed", "err", err.Error())
	}
	_ = r.Jobs.RecomputeJob(ctx, job.ID)
}

// heartbeat keeps the lease and the DB heartbeat alive; it also watches for
// ownership loss (reaper) and cooperative cancel.
func (r *Runner) heartbeat(ctx context.Context, receipt queue.Receipt, taskID, runnerID string, log *slog.Logger) {
	ticker := time.NewTicker(r.Opts.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ok, err := r.Jobs.Heartbeat(ctx, taskID, runnerID)
		if err != nil || !ok {
			log.WarnContext(ctx, "ownership lost (reaped?)", "err", fmt.Sprint(err))
			return
		}
		if err := r.Queue.ExtendVisibility(ctx, receipt, r.Opts.Visibility); err != nil {
			log.WarnContext(ctx, "extend visibility failed", "err", err.Error())
		}
		if cancelReq, _ := r.Jobs.IsCancelRequested(ctx, taskID); cancelReq {
			log.InfoContext(ctx, "cancel requested")
			return
		}
	}
}

func (r *Runner) reportDepth(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if r.Metrics == nil {
			return
		}
		if d, err := r.Queue.Depth(ctx, QueueTasks); err == nil {
			r.Metrics.QueueDepth.WithLabelValues(QueueTasks).Set(float64(d))
		}
	}
}

func (r *Runner) observe(ctx context.Context, job *store.Job, outcome string) {
	if r.Metrics == nil {
		return
	}
	state := outcome
	switch outcome {
	case "retry":
		state = "pending"
	}
	r.Metrics.TasksTotal.WithLabelValues(job.Type, state).Inc()
}

func classified(err error) store.ErrorInfo { return Classified(err) }

func mustAck(ctx context.Context, q queue.TaskQueue, r queue.Receipt, log *slog.Logger) {
	if err := q.Ack(ctx, r); err != nil {
		log.ErrorContext(ctx, "ack failed", "err", err.Error())
	}
}

// event appends an observable fact for SSE consumers. Best-effort: event
// loss never fails the transition it describes.
func (r *Runner) event(ctx context.Context, task *store.Task, typ string, payload map[string]any) {
	if r.Events == nil {
		return
	}
	r.Events.Append(ctx, "task", task.ID, typ, payload)
}
