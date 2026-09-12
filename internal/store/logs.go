package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// TaskLogRepo persists task execution log lines (docs/08-data-model.md §2).
// It is the storage half of the log dual-write (docs/02-architecture.md
// §5.2): records that carry task_id land here at write time, next to the
// local slog output, so an operator can replay one task's trail via the API.
type TaskLogRepo struct{ db *sql.DB }

func NewTaskLogRepo(db *sql.DB) *TaskLogRepo { return &TaskLogRepo{db: db} }

// AppendTaskLog records one line. Best-effort, mirroring EventRepo.Append:
// log loss must never fail the step it describes. Implements the sink side
// of obs.TaskLogTee.
func (r *TaskLogRepo) AppendTaskLog(ctx context.Context, taskID, level, stage, message string, attrs map[string]any) {
	var a any
	if len(attrs) > 0 {
		a, _ = json.Marshal(attrs)
	}
	_, _ = r.db.ExecContext(ctx, `
		INSERT INTO task_logs (task_id, level, stage, message, attrs)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)`, taskID, level, stage, message, a)
}

// TaskLogRecord is one persisted execution line.
type TaskLogRecord struct {
	ID      int64           `json:"id"`
	TaskID  string          `json:"-"`
	Level   string          `json:"level"`
	Stage   string          `json:"stage,omitempty"`
	Message string          `json:"message"`
	Attrs   json.RawMessage `json:"attrs,omitempty"`
	TS      time.Time       `json:"ts"`
}

// TaskLogFilter narrows a task's log query; zero AfterID means "from the
// beginning" (docs/03-api.md §1 cursor pagination).
type TaskLogFilter struct {
	TaskID  string
	AfterID int64
	Limit   int
}

// List returns one task's lines in write order.
func (r *TaskLogRepo) List(ctx context.Context, f TaskLogFilter) ([]TaskLogRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	query := `
		SELECT id, task_id, level, COALESCE(stage, ''), message, attrs, ts
		FROM task_logs WHERE task_id = $1`
	args := []any{f.TaskID}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		query += fmt.Sprintf(" AND id > $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskLogRecord
	for rows.Next() {
		var l TaskLogRecord
		var attrs []byte
		if err := rows.Scan(&l.ID, &l.TaskID, &l.Level, &l.Stage, &l.Message, &attrs, &l.TS); err != nil {
			return nil, err
		}
		if len(attrs) > 0 {
			l.Attrs = json.RawMessage(attrs)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ExpireTaskLogs drops lines older than the retention window; the reaper
// calls this alongside idempotency-key expiry.
func (r *TaskLogRepo) ExpireTaskLogs(ctx context.Context, ttl time.Duration) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM task_logs
		WHERE ts < now() - make_interval(secs => $1)`, ttl.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
