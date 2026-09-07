package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
