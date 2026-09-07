package queue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Options tune dead-lettering and retry pacing.
type Options struct {
	// MaxReceiveCount moves a message to dead-letter after this many
	// deliveries without Ack (default 5).
	MaxReceiveCount int
	// RetryBackoff is the base redelivery delay after Nack; it doubles per
	// delivery count (5s → 10s → 20s …) up to a 5-minute cap.
	RetryBackoff time.Duration
}

func (o Options) maxReceive() int {
	if o.MaxReceiveCount <= 0 {
		return 5
	}
	return o.MaxReceiveCount
}

func (o Options) backoff() time.Duration {
	if o.RetryBackoff <= 0 {
		return 5 * time.Second
	}
	return o.RetryBackoff
}

// PGQueue is the production TaskQueue over a PostgreSQL table. Dequeue claims
// with FOR UPDATE SKIP LOCKED: concurrent runners never block each other, and
// the lease update rides the row lock in one statement — no separate lock
// service needed (docs/10-tech-stack.md D2/D3).
type PGQueue struct {
	db *sql.DB
	oc Options
}

func NewPG(db *sql.DB, opts Options) *PGQueue { return &PGQueue{db: db, oc: opts} }

func (q *PGQueue) Enqueue(ctx context.Context, queue string, payload []byte, opts EnqueueOptions) error {
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO queue_messages (queue, payload, visible_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))`,
		queue, payload, opts.Delay.Seconds())
	return err
}

func (q *PGQueue) Dequeue(ctx context.Context, queue string, visibilityTimeout time.Duration) (Receipt, error) {
	token, err := newToken()
	if err != nil {
		return Receipt{}, err
	}
	const claim = `
		UPDATE queue_messages SET
		  lease_token = $2,
		  lease_expired_at = now() + make_interval(secs => $3),
		  delivery_count = delivery_count + 1
		WHERE id = (
		  SELECT id FROM queue_messages
		  WHERE queue = $1 AND dead = false
		    AND visible_at <= now()
		    AND (lease_expired_at IS NULL OR lease_expired_at <= now())
		  ORDER BY id
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1
		)
		RETURNING id, payload`
	var id int64
	var payload []byte
	err = q.db.QueryRowContext(ctx, claim, queue, token, visibilityTimeout.Seconds()).Scan(&id, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, fmt.Errorf("%w: %s", ErrEmpty, queue)
	}
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Queue: queue, MessageID: id, Token: token, Payload: payload}, nil
}

func (q *PGQueue) Ack(ctx context.Context, r Receipt) error {
	res, err := q.db.ExecContext(ctx, `
		DELETE FROM queue_messages WHERE id = $1 AND lease_token = $2`, r.MessageID, r.Token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (q *PGQueue) Nack(ctx context.Context, r Receipt, reason error) error {
	deliveries, err := q.nack(ctx, r, reason)
	if err != nil {
		return err
	}
	_ = deliveries
	return nil
}

func (q *PGQueue) nack(ctx context.Context, r Receipt, reason error) (int, error) {
	backoff := q.oc.backoff()
	const claim = `
		UPDATE queue_messages SET
		  lease_token = NULL,
		  lease_expired_at = NULL,
		  visible_at = now() + make_interval(secs => $3),
		  dead = (delivery_count >= $4),
		  dead_reason = CASE WHEN delivery_count >= $4 THEN $5 ELSE dead_reason END
		WHERE id = $1 AND lease_token = $2
		RETURNING delivery_count`
	var deliveries int
	err := q.db.QueryRowContext(ctx, claim,
		r.MessageID, r.Token, backoff.Seconds(), q.oc.maxReceive(), reasonText(reason)).
		Scan(&deliveries)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return deliveries, err
}

func (q *PGQueue) ExtendVisibility(ctx context.Context, r Receipt, d time.Duration) error {
	res, err := q.db.ExecContext(ctx, `
		UPDATE queue_messages
		SET lease_expired_at = now() + make_interval(secs => $3)
		WHERE id = $1 AND lease_token = $2`, r.MessageID, r.Token, d.Seconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (q *PGQueue) Depth(ctx context.Context, queue string) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `
		SELECT count(*) FROM queue_messages WHERE queue = $1 AND dead = false`, queue).Scan(&n)
	return n, err
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("queue: token entropy: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func reasonText(reason error) string {
	if reason == nil {
		return ""
	}
	return reason.Error()
}

var _ TaskQueue = (*PGQueue)(nil)
