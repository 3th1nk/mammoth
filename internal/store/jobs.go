package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JobRepo persists jobs, tasks and stages. Job creation and task mutation
// shape the consistency rules from docs/08-data-model.md:
//   - creation of a job, its tasks and stage rows is one transaction;
//   - task state advances only via CAS updates (optimistic lock on
//     stage_index/state), so a stale owner can never resurrect progress;
//   - job summary is materialized from tasks, never incremented piecemeal.
type JobRepo struct{ db *sql.DB }

func NewJobRepo(db *sql.DB) *JobRepo { return &JobRepo{db: db} }

// CreateJobInput is everything a creating POST writes in one transaction.
type CreateJobInput struct {
	Job        *Job
	TaskIDs    []string // parallel to MachineIDs; task i belongs to machine i
	MachineIDs []string
	Stages     []string // flow stage names, persisted per task (docs/02-architecture.md §2.1)
}

func (r *JobRepo) CreateJobWithTasks(ctx context.Context, in CreateJobInput) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	policy, _ := json.Marshal(in.Job.Policy)
	summary, _ := json.Marshal(DefaultSummary(len(in.MachineIDs)))
	request := in.Job.Request
	if request == nil {
		request = json.RawMessage(`{}`)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs (id, type, request, action, spec_resolved, policy, idempotency_key, state, summary, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',$8,$9)`,
		in.Job.ID, in.Job.Type, request, in.Job.Action, in.Job.SpecResolved,
		policy, in.Job.IdempotencyKey, summary, in.Job.CreatedBy)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: idempotency key replay", ErrIdempotentHit)
		}
		return err
	}

	for i, mid := range in.MachineIDs {
		tid := in.TaskIDs[i]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (id, job_id, machine_id, state, flow_name)
			VALUES ($1,$2,$3,'pending',$4)`, tid, in.Job.ID, mid, in.Job.FlowName()); err != nil {
			return err
		}
		for seq, name := range in.Stages {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO task_stages (task_id, seq, name, state) VALUES ($1,$2,$3,'pending')`,
				tid, seq, name); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// DefaultSummary returns the initial per-state task counts.
func DefaultSummary(total int) map[string]int {
	return map[string]int{"total": total, "pending": total, "running": 0,
		"succeeded": 0, "failed": 0, "interrupted": 0, "canceled": 0}
}

// GetIdempotentJob returns the job previously created under the key, if any.
func (r *JobRepo) GetByIdempotencyKey(ctx context.Context, key string) (*Job, error) {
	row := r.db.QueryRowContext(ctx, jobSelect+` WHERE idempotency_key = $1`, key)
	j, err := scanJob(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return j, err
}

func (r *JobRepo) GetJob(ctx context.Context, id string) (*Job, error) {
	row := r.db.QueryRowContext(ctx, jobSelect+` WHERE id = $1`, id)
	return scanJob(row)
}

// JobListFilter narrows job listings.
type JobListFilter struct {
	Type, State string
	PageSize    int
	Cursor      *Cursor
	Order       string
}

func (r *JobRepo) ListJobs(ctx context.Context, f JobListFilter) ([]*Job, *Cursor, error) {
	where, args := []string{"1=1"}, []any{}
	if f.Type != "" {
		args = append(args, f.Type)
		where = append(where, fmt.Sprintf("type = $%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		where = append(where, fmt.Sprintf("state = $%d", len(args)))
	}
	jobs, next, err := r.paginate(ctx, jobSelect, "jobs", where, args, f.PageSize, f.Cursor, f.Order)
	return jobs, next, err
}

// MachineIDs returns the target machines of a job in task creation order.
func (r *JobRepo) MachineIDs(ctx context.Context, jobID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT machine_id FROM tasks WHERE job_id = $1 ORDER BY created_at, id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *JobRepo) GetTask(ctx context.Context, id string) (*Task, error) {
	row := r.db.QueryRowContext(ctx, taskSelect+` WHERE id = $1`, id)
	return scanTask(row)
}

// TaskListFilter narrows task listings.
type TaskListFilter struct {
	JobID    string
	State    string
	PageSize int
	Cursor   *Cursor
	Order    string
}

func (r *JobRepo) ListTasks(ctx context.Context, f TaskListFilter) ([]*Task, *Cursor, error) {
	where, args := []string{"job_id = $1"}, []any{f.JobID}
	if f.State != "" {
		args = append(args, f.State)
		where = append(where, fmt.Sprintf("state = $%d", len(args)))
	}
	tasks, next, err := r.paginateTasks(ctx, where, args, f.PageSize, f.Cursor, f.Order)
	return tasks, next, err
}

// Stages returns the stage rows of a task ordered by seq.
func (r *JobRepo) Stages(ctx context.Context, taskID string) ([]*Stage, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT task_id, seq, name, state, attempt, started_at, finished_at, duration_ms
		FROM task_stages WHERE task_id = $1 ORDER BY seq`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Stage
	for rows.Next() {
		var s Stage
		var started, finished sql.NullTime
		var dur sql.NullInt64
		if err := rows.Scan(&s.TaskID, &s.Seq, &s.Name, &s.State, &s.Attempt,
			&started, &finished, &dur); err != nil {
			return nil, err
		}
		if started.Valid {
			t := started.Time
			s.StartedAt = &t
		}
		if finished.Valid {
			t := finished.Time
			s.FinishedAt = &t
		}
		if dur.Valid {
			v := dur.Int64
			s.DurationMs = &v
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// ── task lifecycle: CAS transitions owned by the runner ────────────────────

// ClaimTask marks a queued message's task running by its owner. The CAS on
// state means: a task already reaped (interrupted) or canceled rejects the
// claim — the caller then Acks the stale message and moves on.
func (r *JobRepo) ClaimTask(ctx context.Context, taskID, runnerID string, visibility time.Duration) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
		  state = 'running', owner_runner = $2, heartbeat_at = now(),
		  stage_deadline = now() + make_interval(secs => $3),
		  updated_at = now()
		WHERE id = $1 AND state = 'pending'`,
		taskID, runnerID, visibility.Seconds())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StealInterruptedTask re-claims an interrupted task for its retry. Retry is
// an explicit API action; a late redelivery of the old queue message hits the
// state guard and does NOT revive the task.
func (r *JobRepo) StealInterruptedTask(ctx context.Context, taskID, runnerID string, visibility time.Duration) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
		  state = 'running', owner_runner = $2, heartbeat_at = now(),
		  error = NULL, cancel_requested = false,
		  stage_deadline = now() + make_interval(secs => $3),
		  updated_at = now()
		WHERE id = $1 AND state = 'interrupted'`,
		taskID, runnerID, visibility.Seconds())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Heartbeat refreshes ownership (docs/08-data-model.md iron rule 3).
// Returns false when ownership was lost (reaped) — the runner must stop.
func (r *JobRepo) Heartbeat(ctx context.Context, taskID, runnerID string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET heartbeat_at = now(), updated_at = now()
		WHERE id = $1 AND owner_runner = $2 AND state = 'running'`, taskID, runnerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// IsCancelRequested reads the cooperative cancel flag.
func (r *JobRepo) IsCancelRequested(ctx context.Context, taskID string) (bool, error) {
	var v bool
	err := r.db.QueryRowContext(ctx,
		`SELECT cancel_requested FROM tasks WHERE id = $1`, taskID).Scan(&v)
	return v, err
}

// AdvanceStage marks stage seq done and moves the task to seq+1 (or leaves
// the task at the final stage for completion). CAS on stage_index prevents
// concurrent/late owners from skipping stages (docs/02-architecture.md §2.6).
func (r *JobRepo) AdvanceStage(ctx context.Context, taskID string, fromSeq int) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE task_stages SET state='succeeded', finished_at = now(),
		  duration_ms = (extract(epoch from (now() - started_at)) * 1000)::int
		WHERE task_id = $1 AND seq = $2 AND state <> 'succeeded'`, taskID, fromSeq)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		UPDATE tasks SET stage_index = $2, stage_attempt = 0, updated_at = now()
		WHERE id = $1 AND stage_index = $2 - 1`, taskID, fromSeq+1)
	return err
}

// StartStage marks the current stage running.
func (r *JobRepo) StartStage(ctx context.Context, taskID string, seq int) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE task_stages SET state='running', started_at = COALESCE(started_at, now()),
		  attempt = attempt + 1
		WHERE task_id = $1 AND seq = $2`, taskID, seq)
	return err
}

// FailStage marks the current stage failed.
func (r *JobRepo) FailStage(ctx context.Context, taskID string, seq int) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE task_stages SET state='failed', finished_at = now(),
		  duration_ms = (extract(epoch from (now() - started_at)) * 1000)::int
		WHERE task_id = $1 AND seq = $2`, taskID, seq)
	return err
}

// CompleteTask finishes a task successfully.
func (r *JobRepo) CompleteTask(ctx context.Context, taskID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET state='succeeded', stage_attempt = stage_attempt + 1,
		  finished_at = now(), updated_at = now(), error = NULL
		WHERE id = $1`, taskID)
	return err
}

// FailTask finishes a task with a classified error (terminal for this attempt
// chain: the retry ceiling was reached or the error is non-retryable).
func (r *JobRepo) FailTask(ctx context.Context, taskID string, errInfo ErrorInfo) error {
	b, _ := json.Marshal(errInfo)
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET state='failed', error = $2, finished_at = now(), updated_at = now()
		WHERE id = $1`, taskID, b)
	return err
}

// BumpAttempt records a retryable failure: the task goes back to pending for
// another delivery, attempt count carried in stage_attempt.
func (r *JobRepo) BumpAttempt(ctx context.Context, taskID string, errInfo ErrorInfo) error {
	b, _ := json.Marshal(errInfo)
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET state='pending', owner_runner = NULL, heartbeat_at = NULL,
		  stage_attempt = stage_attempt + 1, error = $2, updated_at = now()
		WHERE id = $1 AND state = 'running'`, taskID, b)
	return err
}

// MarkCanceled is the one write the control plane may perform directly
// (docs/08-data-model.md iron rule 1).
func (r *JobRepo) MarkCanceled(ctx context.Context, taskID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET state='canceled', finished_at = now(), updated_at = now()
		WHERE id = $1 AND state IN ('pending', 'interrupted')`, taskID)
	return err
}

// RequestCancel flags a running task; the owner honors it at its next check
// and performs compensation.
func (r *JobRepo) RequestCancel(ctx context.Context, taskID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET cancel_requested = true, updated_at = now() WHERE id = $1`, taskID)
	return err
}

// InterruptTask is the reaper's single sanctioned write: heartbeat lost →
// interrupted, ownership released, retryable.
func (r *JobRepo) InterruptTask(ctx context.Context, taskID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET state='interrupted', owner_runner = NULL, updated_at = now()
		WHERE id = $1 AND state = 'running'`, taskID)
	return err
}

// InterruptStaleTasks reaps every running task whose heartbeat is older than
// the timeout and returns the reaped ids.
func (r *JobRepo) InterruptStaleTasks(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE tasks SET state='interrupted', owner_runner = NULL, updated_at = now()
		WHERE state = 'running'
		  AND heartbeat_at IS NOT NULL
		  AND heartbeat_at < now() - make_interval(secs => $1)
		RETURNING id`, olderThan.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CancelPendingTasks cancels all non-terminal tasks of a job (control-plane
// cancel write) and flags running ones for cooperative cancel by their owner.
// Returns the flagged (still running) task ids.
func (r *JobRepo) CancelJobTasks(ctx context.Context, jobID string) ([]string, error) {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET state='canceled', finished_at = now(), updated_at = now()
		WHERE job_id = $1 AND state IN ('pending', 'interrupted')`, jobID); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		UPDATE tasks SET cancel_requested = true, updated_at = now()
		WHERE job_id = $1 AND state = 'running' RETURNING id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ── job aggregation ─────────────────────────────────────────────────────────

// RecomputeJob refreshes the materialized summary and derives the job state.
// Called by the runner on every terminal transition; single UPDATE, no read-
// modify-write races with itself (last writer wins with identical data).
func (r *JobRepo) RecomputeJob(ctx context.Context, jobID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var s struct{ Total, Pending, Running, Succeeded, Failed, Interrupted, Canceled int }
	err = tx.QueryRowContext(ctx, `
		SELECT count(*),
		  count(*) FILTER (WHERE state = 'pending'),
		  count(*) FILTER (WHERE state = 'running'),
		  count(*) FILTER (WHERE state = 'succeeded'),
		  count(*) FILTER (WHERE state = 'failed'),
		  count(*) FILTER (WHERE state = 'interrupted'),
		  count(*) FILTER (WHERE state = 'canceled')
		FROM tasks WHERE job_id = $1`, jobID).
		Scan(&s.Total, &s.Pending, &s.Running, &s.Succeeded, &s.Failed, &s.Interrupted, &s.Canceled)
	if err != nil {
		return err
	}

	state := "running"
	allSettled := s.Running == 0 && s.Pending == 0 && s.Interrupted == 0
	switch {
	case s.Pending == s.Total:
		state = "pending"
	case !allSettled:
		// Interrupted tasks are retryable, not terminal: the job stays
		// non-terminal while anything remains un-resolved.
		state = "running"
	default:
		switch {
		case s.Succeeded == s.Total:
			state = "succeeded"
		case s.Canceled > 0 && s.Failed == 0:
			state = "canceled"
		case s.Failed == s.Total:
			state = "failed"
		default:
			state = "partial"
		}
	}

	summary, _ := json.Marshal(map[string]int{
		"total": s.Total, "pending": s.Pending, "running": s.Running,
		"succeeded": s.Succeeded, "failed": s.Failed,
		"interrupted": s.Interrupted, "canceled": s.Canceled,
	})

	var finished any
	if allSettled {
		finished = time.Now()
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET summary = $2, state = $3, finished_at = COALESCE($4, finished_at), updated_at = now()
		WHERE id = $1`, jobID, summary, state, finished)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ExpireIdempotencyKeys drops replay keys older than the window.
func (r *JobRepo) ExpireIdempotencyKeys(ctx context.Context, ttl time.Duration) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE jobs SET idempotency_key = NULL
		WHERE idempotency_key IS NOT NULL AND created_at < now() - make_interval(secs => $1)`,
		ttl.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ── scan helpers ────────────────────────────────────────────────────────────

const jobSelect = `
	SELECT id, type, request, action, spec_resolved, policy, idempotency_key, state, summary, created_by, created_at, updated_at, finished_at
	FROM jobs`

const taskSelect = `
	SELECT id, job_id, machine_id, state, flow_name, stage_index, stage_attempt, delivery_count,
	       stage_deadline, heartbeat_at, owner_runner, context, cancel_requested, error, created_at, updated_at, finished_at
	FROM tasks`

type scanner interface{ Scan(dest ...any) error }

func scanJob(row scanner) (*Job, error) {
	var j Job
	var request, action, spec, policy, summary []byte
	var idem sql.NullString
	var finished sql.NullTime
	err := row.Scan(&j.ID, &j.Type, &request, &action, &spec, &policy, &idem,
		&j.State, &summary, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.Request = json.RawMessage(request)
	if len(action) > 0 {
		j.Action = json.RawMessage(action)
	}
	if len(spec) > 0 {
		j.SpecResolved = json.RawMessage(spec)
	}
	_ = json.Unmarshal(policy, &j.Policy)
	if idem.Valid {
		v := idem.String
		j.IdempotencyKey = &v
	}
	_ = json.Unmarshal(summary, &j.Summary)
	if finished.Valid {
		t := finished.Time
		j.FinishedAt = &t
	}
	return &j, nil
}

func scanTask(row scanner) (*Task, error) {
	var t Task
	var contextRaw, errRaw []byte
	var deadline, heartbeat, finished sql.NullTime
	var owner sql.NullString
	var cancel bool
	err := row.Scan(&t.ID, &t.JobID, &t.MachineID, &t.State, &t.FlowName, &t.StageIndex,
		&t.StageAttempt, &t.DeliveryCount, &deadline, &heartbeat, &owner, &contextRaw,
		&cancel, &errRaw, &t.CreatedAt, &t.UpdatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if deadline.Valid {
		v := deadline.Time
		t.StageDeadline = &v
	}
	if heartbeat.Valid {
		v := heartbeat.Time
		t.HeartbeatAt = &v
	}
	if owner.Valid {
		v := owner.String
		t.OwnerRunner = &v
	}
	t.Context = json.RawMessage(contextRaw)
	t.CancelRequested = cancel
	if len(errRaw) > 0 {
		var ei ErrorInfo
		if json.Unmarshal(errRaw, &ei) == nil {
			t.Error = &ei
		}
	}
	if finished.Valid {
		v := finished.Time
		t.FinishedAt = &v
	}
	return &t, nil
}

// paginate is the shared keyset pagination over (created_at, id).
func (r *JobRepo) paginate(ctx context.Context, selectSQL, table string, where []string, args []any, pageSize int, cursor *Cursor, order string) ([]*Job, *Cursor, error) {
	if cursor != nil {
		args = append(args, cursor.CreatedAt, cursor.ID)
		op := "<"
		if order == "asc" {
			op = ">"
		}
		where = append(where, fmt.Sprintf("(created_at, id) %s ($%d, $%d)", op, len(args)-1, len(args)))
	}
	if order != "asc" {
		order = "desc"
	}
	limit := pageSize
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit+1)
	rows, err := r.db.QueryContext(ctx, selectSQL+
		" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY created_at "+order+", id "+order+
		fmt.Sprintf(" LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, j)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return items, next, nil
}

func (r *JobRepo) paginateTasks(ctx context.Context, where []string, args []any, pageSize int, cursor *Cursor, order string) ([]*Task, *Cursor, error) {
	if cursor != nil {
		args = append(args, cursor.CreatedAt, cursor.ID)
		op := "<"
		if order == "asc" {
			op = ">"
		}
		where = append(where, fmt.Sprintf("(created_at, id) %s ($%d, $%d)", op, len(args)-1, len(args)))
	}
	if order != "asc" {
		order = "desc"
	}
	limit := pageSize
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit+1)
	rows, err := r.db.QueryContext(ctx, taskSelect+
		" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY created_at "+order+", id "+order+
		fmt.Sprintf(" LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return items, next, nil
}

// ResetForRetry re-arms a failed/interrupted task for another delivery: it
// goes back to pending with ownership cleared; the stage index is preserved
// so retry re-enters at the last stage (docs/09-roadmap.md: 可重试).
func (r *JobRepo) ResetForRetry(ctx context.Context, taskID string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
		  state = 'pending', owner_runner = NULL, heartbeat_at = NULL,
		  cancel_requested = false, error = NULL, finished_at = NULL,
		  stage_attempt = 0, updated_at = now()
		WHERE id = $1 AND state IN ('failed', 'interrupted')`, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}
