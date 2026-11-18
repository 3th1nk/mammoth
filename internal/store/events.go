package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EventRepo appends observable facts (stage transitions, terminal states).
// SSE delivery lands later; persistence comes first so the stream has a
// waterline to replay from (docs/08-data-model.md).
type EventRepo struct{ db *sql.DB }

func NewEventRepo(db *sql.DB) *EventRepo { return &EventRepo{db: db} }

// Append records one event. Best-effort in callers: event loss must never
// fail the state transition it describes.
func (r *EventRepo) Append(ctx context.Context, resourceType, resourceID, typ string, payload any) {
	var p any
	if payload != nil {
		p, _ = json.Marshal(payload)
	}
	_, _ = r.db.ExecContext(ctx, `
		INSERT INTO events (resource_type, resource_id, type, payload)
		VALUES ($1, $2, $3, $4)`, resourceType, resourceID, typ, p)
}

// EventRecord is one observable fact (docs/08-data-model.md; SSE watermark
// by monotonic id — docs/03-api.md §4).
type EventRecord struct {
	ID           int64           `json:"id"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	TS           time.Time       `json:"ts"`
}

// EventFilter narrows event queries. Asc (default) is watermark order with
// AfterID as the exclusive lower bound — zero means "from the beginning".
// Desc walks backward from the newest event with BeforeID as the exclusive
// upper bound — how clients read the latest row (page_size=1 watermark) and
// page an audit view newest-first (docs/03-api.md list order).
type EventFilter struct {
	AfterID      int64
	BeforeID     int64
	Desc         bool
	ResourceType string
	ResourceID   string
	Type         string
	Limit        int
}

// List queries events in the filter's order (watermark order asc by
// default; newest first under Desc).
func (r *EventRepo) List(ctx context.Context, f EventFilter) ([]EventRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	where, args := []string{"1=1"}, []any{}
	if f.Desc {
		if f.BeforeID > 0 {
			args = append(args, f.BeforeID)
			where = append(where, fmt.Sprintf("id < $%d", len(args)))
		}
	} else if f.AfterID > 0 {
		args = append(args, f.AfterID)
		where = append(where, fmt.Sprintf("id > $%d", len(args)))
	}
	if f.ResourceType != "" {
		args = append(args, f.ResourceType)
		where = append(where, fmt.Sprintf("resource_type = $%d", len(args)))
	}
	if f.ResourceID != "" {
		args = append(args, f.ResourceID)
		where = append(where, fmt.Sprintf("resource_id = $%d", len(args)))
	}
	if f.Type != "" {
		args = append(args, f.Type)
		where = append(where, fmt.Sprintf("type = $%d", len(args)))
	}
	args = append(args, limit)
	order := "ASC"
	if f.Desc {
		order = "DESC"
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, resource_type, resource_id, type, payload, ts
		FROM events WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id `+order+` LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var e EventRecord
		var payload []byte
		if err := rows.Scan(&e.ID, &e.ResourceType, &e.ResourceID, &e.Type, &payload, &e.TS); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
