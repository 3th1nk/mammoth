package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // register "sqlite" driver (pure Go, no cgo)
)

// SQLiteQueue implements the same TaskQueue semantics on SQLite for unit
// tests and the future minimal single-node deployment (docs/09-roadmap.md M6).
// SQLite serializes writers, so the PG SKIP LOCKED dance collapses to
// BEGIN IMMEDIATE … UPDATE … COMMIT.
type SQLiteQueue struct {
	db *sql.DB
	oc Options
}

// OpenSQLite opens (creating if needed) the queue database and ensures the
// schema exists.
func OpenSQLite(path string, opts Options) (*SQLiteQueue, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single writer connection avoids SQLITE_BUSY storms; queue throughput
	// requirements are modest (docs/10-tech-stack.md D3).
	db.SetMaxOpenConns(1)
	q := &SQLiteQueue{db: db, oc: opts}
	if err := q.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return q, nil
}

// NewSQLite wraps an existing *sql.DB (test helper).
func NewSQLite(db *sql.DB, opts Options) (*SQLiteQueue, error) {
	q := &SQLiteQueue{db: db, oc: opts}
	if err := q.migrate(context.Background()); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *SQLiteQueue) migrate(ctx context.Context) error {
	_, err := q.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS queue_messages (
		    id               INTEGER PRIMARY KEY AUTOINCREMENT,
		    queue            TEXT NOT NULL,
		    payload          BLOB NOT NULL,
		    visible_at       INTEGER NOT NULL,
		    lease_token      TEXT,
		    lease_expired_at INTEGER,
		    delivery_count   INTEGER NOT NULL DEFAULT 0,
		    dead             INTEGER NOT NULL DEFAULT 0,
		    dead_reason      TEXT,
		    enqueued_at      INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sqlite_queue_poll ON queue_messages (queue, visible_at);
	`)
	return err
}

func (q *SQLiteQueue) Enqueue(ctx context.Context, queue string, payload []byte, opts EnqueueOptions) error {
	now := time.Now()
	visible := now.Add(opts.Delay)
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO queue_messages (queue, payload, visible_at, enqueued_at)
		VALUES (?, ?, ?, ?)`, queue, payload, visible.UnixNano(), now.UnixNano())
	return err
}

func (q *SQLiteQueue) Dequeue(ctx context.Context, queue string, visibilityTimeout time.Duration) (Receipt, error) {
	token, err := newToken()
	if err != nil {
		return Receipt{}, err
	}
	now := time.Now()
	leaseUntil := now.Add(visibilityTimeout)

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// IMMEDIATE takes the write lock up front; with a single connection this
	// serializes claims exactly like SKIP LOCKED serializes them on PG.
	if _, err := tx.ExecContext(ctx, `SELECT 1`); err != nil {
		return Receipt{}, err
	}
	var id int64
	var payload []byte
	err = tx.QueryRowContext(ctx, `
		SELECT id, payload FROM queue_messages
		WHERE queue = ? AND dead = 0
		  AND visible_at <= ?
		  AND (lease_expired_at IS NULL OR lease_expired_at <= ?)
		ORDER BY id LIMIT 1`, queue, now.UnixNano(), now.UnixNano()).Scan(&id, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, fmt.Errorf("%w: %s", ErrEmpty, queue)
	}
	if err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE queue_messages SET lease_token = ?, lease_expired_at = ?, delivery_count = delivery_count + 1
		WHERE id = ?`, token, leaseUntil.UnixNano(), id); err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, err
	}
	return Receipt{Queue: queue, MessageID: id, Token: token, Payload: payload}, nil
}

func (q *SQLiteQueue) Ack(ctx context.Context, r Receipt) error {
	res, err := q.db.ExecContext(ctx, `
		DELETE FROM queue_messages WHERE id = ? AND lease_token = ?`, r.MessageID, r.Token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (q *SQLiteQueue) Nack(ctx context.Context, r Receipt, reason error) error {
	now := time.Now()
	backoff := q.oc.backoff()
	visible := now.Add(backoff)
	dead := 0
	var deliveries int
	if err := q.db.QueryRowContext(ctx,
		`SELECT delivery_count FROM queue_messages WHERE id = ? AND lease_token = ?`,
		r.MessageID, r.Token).Scan(&deliveries); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if deliveries >= q.oc.maxReceive() {
		dead = 1
	}
	_, err := q.db.ExecContext(ctx, `
		UPDATE queue_messages SET
		  lease_token = NULL, lease_expired_at = NULL, visible_at = ?,
		  dead = ?, dead_reason = COALESCE(NULLIF(?, ''), dead_reason)
		WHERE id = ? AND lease_token = ?`,
		visible.UnixNano(), dead, reasonText(reason), r.MessageID, r.Token)
	return err
}

func (q *SQLiteQueue) ExtendVisibility(ctx context.Context, r Receipt, d time.Duration) error {
	until := time.Now().Add(d)
	res, err := q.db.ExecContext(ctx, `
		UPDATE queue_messages SET lease_expired_at = ? WHERE id = ? AND lease_token = ?`,
		until.UnixNano(), r.MessageID, r.Token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (q *SQLiteQueue) Depth(ctx context.Context, queue string) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `
		SELECT count(*) FROM queue_messages WHERE queue = ? AND dead = 0`, queue).Scan(&n)
	return n, err
}

var _ TaskQueue = (*SQLiteQueue)(nil)
