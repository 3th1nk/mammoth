package queue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// runSuites executes the shared contract suite against a queue implementation.
// Both implementations must satisfy identical semantics (docs/10-tech-stack.md
// D3: the abstraction boundary is the spec).
func runSuites(t *testing.T, name string, newQ func(t *testing.T) TaskQueue) {
	// Each subtest gets a distinct queue name: implementations share one
	// table/file, and leftover messages from earlier subtests (e.g. forged
	// receipts that stay leased) must not pollute depth assertions.
	qn := func(t *testing.T) string { return name + "/" + t.Name() }

	t.Run(name+"/fifo-order", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		for _, m := range []string{"m1", "m2", "m3"} {
			must(t, q.Enqueue(ctx, qq, []byte(m), EnqueueOptions{}))
		}
		for _, want := range []string{"m1", "m2", "m3"} {
			r, err := q.Dequeue(ctx, qq, time.Minute)
			if err != nil {
				t.Fatalf("dequeue: %v", err)
			}
			if got := string(r.Payload); got != want {
				t.Fatalf("order: got %s want %s", got, want)
			}
			must(t, q.Ack(ctx, r))
		}
		if _, err := q.Dequeue(ctx, qq, time.Minute); !isEmpty(err) {
			t.Fatalf("want empty, got %v", err)
		}
	})

	t.Run(name+"/ack-removes", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		must(t, q.Enqueue(ctx, qq, []byte("x"), EnqueueOptions{}))
		r, err := q.Dequeue(ctx, qq, time.Minute)
		must(t, err)
		must(t, q.Ack(ctx, r))
		// Acked message never comes back.
		shortCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if _, err := q.Dequeue(shortCtx, qq, time.Minute); !isEmpty(err) {
			t.Fatalf("acked message redelivered: %v", err)
		}
	})

	t.Run(name+"/visibility-expiry-redelivers", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		must(t, q.Enqueue(ctx, qq, []byte("job"), EnqueueOptions{}))
		if _, err := q.Dequeue(ctx, qq, 50*time.Millisecond); err != nil {
			t.Fatalf("first dequeue: %v", err)
		}
		// Lease lapses without Ack → redelivery (at-least-once).
		deadline := time.Now().Add(3 * time.Second)
		for {
			r, err := q.Dequeue(ctx, qq, time.Minute)
			if err == nil {
				must(t, q.Ack(ctx, r))
				return
			}
			if !errors.Is(err, ErrEmpty) {
				t.Fatalf("dequeue: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatal("message never redelivered after visibility expiry")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run(name+"/extend-visibility-holds-lease", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		must(t, q.Enqueue(ctx, qq, []byte("hold"), EnqueueOptions{}))
		r, err := q.Dequeue(ctx, qq, 120*time.Millisecond)
		must(t, err)
		must(t, q.ExtendVisibility(ctx, r, 5*time.Second))
		// Original window passes; renewal keeps it leased.
		shortCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		if _, err := q.Dequeue(shortCtx, qq, time.Minute); !isEmpty(err) {
			t.Fatalf("extended lease was stolen: %v", err)
		}
		must(t, q.Ack(ctx, r))
	})

	t.Run(name+"/stale-receipt-rejected", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		must(t, q.Enqueue(ctx, qq, []byte("a"), EnqueueOptions{}))
		r, _ := q.Dequeue(ctx, qq, time.Minute)
		r.Token = "forged"
		if err := q.Ack(ctx, r); !errors.Is(err, ErrNotFound) {
			t.Fatalf("forged token ack: want ErrNotFound, got %v", err)
		}
	})

	t.Run(name+"/nack-redelivers-then-deadletters", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		must(t, q.Enqueue(ctx, qq, []byte("poison"), EnqueueOptions{}))

		const maxReceive = 3
		for i := 0; i < maxReceive; i++ {
			r, err := q.Dequeue(ctx, qq, time.Minute)
			if err != nil {
				// backoff may hold it briefly
				time.Sleep(30 * time.Millisecond)
				r, err = q.Dequeue(ctx, qq, time.Minute)
				if err != nil {
					t.Fatalf("round %d dequeue: %v", i, err)
				}
			}
			if err := q.Nack(ctx, r, errors.New("boom")); err != nil {
				t.Fatalf("round %d nack: %v", i, err)
			}
		}
		// Max receive count exhausted → dead letter, no further delivery.
		time.Sleep(50 * time.Millisecond)
		shortCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if _, err := q.Dequeue(shortCtx, qq, time.Minute); !isEmpty(err) {
			t.Fatalf("dead-lettered message redelivered: %v", err)
		}
		if d, _ := q.Depth(ctx, qq); d != 0 {
			t.Fatalf("depth counts dead letters: %d", d)
		}
	})

	t.Run(name+"/delay-defers-visibility", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qq := qn(t)
		must(t, q.Enqueue(ctx, qq, []byte("later"), EnqueueOptions{Delay: 400 * time.Millisecond}))
		shortCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		if _, err := q.Dequeue(shortCtx, qq, time.Minute); !isEmpty(err) {
			t.Fatalf("delayed message visible too early: %v", err)
		}
		time.Sleep(600 * time.Millisecond)
		r, err := q.Dequeue(ctx, qq, time.Minute)
		if err != nil {
			t.Fatalf("delayed message not visible after delay: %v", err)
		}
		must(t, q.Ack(ctx, r))
	})

	t.Run(name+"/queues-are-isolated", func(t *testing.T) {
		q := newQ(t)
		ctx := context.Background()
		qa, qb := qn(t)+"-a", qn(t)+"-b"
		must(t, q.Enqueue(ctx, qa, []byte("in-a"), EnqueueOptions{}))
		shortCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if _, err := q.Dequeue(shortCtx, qb, time.Minute); !isEmpty(err) {
			t.Fatalf("cross-queue leak: %v", err)
		}
		r, err := q.Dequeue(ctx, qa, time.Minute)
		must(t, err)
		must(t, q.Ack(ctx, r))
	})
}

// isEmpty accepts ErrEmpty or a context deadline: an empty-queue dequeue
// racing context expiry may surface either, both mean "nothing delivered".
func isEmpty(err error) bool {
	return errors.Is(err, ErrEmpty) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestSQLiteQueue runs the full contract suite on the SQLite implementation.
// This is the M0 unit-test path: no external services (docs/09-roadmap.md M0).
func TestSQLiteQueue(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "queue.db")
	runSuites(t, "sqlite", func(t *testing.T) TaskQueue {
		q, err := OpenSQLite(dbPath, Options{MaxReceiveCount: 3, RetryBackoff: 10 * time.Millisecond})
		if err != nil {
			t.Fatalf("open sqlite queue: %v", err)
		}
		t.Cleanup(func() { _ = q.db.Close() })
		return q
	})
}

// TestPGQueue runs the same suite against PostgreSQL when
// MAMMOTH_TEST_PG_DSN points at a disposable database; it is skipped
// otherwise so `go test ./...` stays dependency-free.
func TestPGQueue(t *testing.T) {
	dsn := testPGDSN()
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping PostgreSQL queue suite")
	}
	runSuites(t, "pg", func(t *testing.T) TaskQueue {
		db := openTestPG(t, dsn)
		t.Cleanup(func() { truncateQueueTables(t, db) })
		return NewPG(db, Options{MaxReceiveCount: 3, RetryBackoff: 10 * time.Millisecond})
	})
}
