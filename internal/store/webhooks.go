package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// WebhookRepo stores event-delivery subscriptions. Secrets are encrypted
// with the same master-key scheme as credentials and never returned after
// creation (docs/03-api.md: write-only secret policy).
type WebhookRepo struct{ db *sql.DB }

func NewWebhookRepo(db *sql.DB) *WebhookRepo { return &WebhookRepo{db: db} }

// Webhook is one subscription.
type Webhook struct {
	ID         string
	URL        string
	SecretEnc  []byte
	Types      []string
	ResourceID string
	Enabled    bool
	Watermark  int64
	FailCount  int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (r *WebhookRepo) Create(ctx context.Context, w *Webhook) error {
	types, _ := json.Marshal(w.Types)
	var rid any
	if w.ResourceID != "" {
		rid = w.ResourceID
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO webhook_subscriptions (id, url, secret_encrypted, types, resource_id)
		VALUES ($1, $2, $3, $4, $5)`,
		w.ID, w.URL, w.SecretEnc, types, rid)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (r *WebhookRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM webhook_subscriptions WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *WebhookRepo) Get(ctx context.Context, id string) (*Webhook, error) {
	row := r.db.QueryRowContext(ctx, webhookSelect+` WHERE id = $1`, id)
	return scanWebhook(row)
}

func (r *WebhookRepo) List(ctx context.Context) ([]*Webhook, error) {
	rows, err := r.db.QueryContext(ctx, webhookSelect+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Webhook
	for rows.Next() {
		e, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const webhookSelect = `
	SELECT id, url, secret_encrypted, types, resource_id, enabled, watermark, fail_count, created_at, updated_at
	FROM webhook_subscriptions`

type webhookScanner interface{ Scan(dest ...any) error }

func scanWebhook(row webhookScanner) (*Webhook, error) {
	var w Webhook
	var types []byte
	var rid sql.NullString
	err := row.Scan(&w.ID, &w.URL, &w.SecretEnc, &types, &rid, &w.Enabled,
		&w.Watermark, &w.FailCount, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(types) > 0 {
		_ = json.Unmarshal(types, &w.Types)
	}
	if rid.Valid {
		w.ResourceID = rid.String
	}
	return &w, nil
}

// AdvanceDelivery records a successful delivery: watermark moves to the
// event id, failure streak resets.
func (r *WebhookRepo) AdvanceDelivery(ctx context.Context, id string, eventID int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE webhook_subscriptions
		SET watermark = $2, fail_count = 0, updated_at = now()
		WHERE id = $1`, id, eventID)
	return err
}

// RecordDeliveryFailure increments the failure streak (input to backoff).
func (r *WebhookRepo) RecordDeliveryFailure(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE webhook_subscriptions
		SET fail_count = fail_count + 1, updated_at = now()
		WHERE id = $1`, id)
	return err
}
