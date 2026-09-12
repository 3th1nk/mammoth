// Package queue defines TaskQueue — the SQS-semantic abstraction over job/task
// instruction queues (docs/10-tech-stack.md D3): at-least-once delivery,
// visibility timeout with heartbeated renewal, Ack/Nack, dead-letter.
//
// Interface discipline: no implementation-specific concept leaks through —
// callers see only queue names and opaque message bytes. PostgreSQL
// (SKIP LOCKED) is the implementation: the queue lives in the same database
// as task state, so dequeue and state transitions share one transaction
// (docs/10-tech-stack.md D3).
package queue

import (
	"context"
	"errors"
	"time"
)

// ErrEmpty is returned (wrapped) by Dequeue when no message is available.
var ErrEmpty = errors.New("queue: empty")

// ErrNotFound marks a stale or foreign receipt: the lease no longer exists
// (expired and re-leased, already acked, or never owned by this caller).
var ErrNotFound = errors.New("queue: receipt not found")

// EnqueueOptions carries the enqueue-time knobs.
type EnqueueOptions struct {
	// Delay defers visibility: the message becomes pollable no earlier than
	// now+Delay (used for retry backoff).
	Delay time.Duration
}

// Receipt proves dequeue ownership and carries the leased message. Only the
// holder of the exact receipt may Ack / Nack / renew; foreign or stale
// receipts are rejected as ErrNotFound.
type Receipt struct {
	Queue     string
	MessageID int64
	Token     string // lease token minted at dequeue
	Payload   []byte // message bytes, delivered with the lease
}

func (r Receipt) Valid() bool { return r.MessageID > 0 && r.Token != "" }

// TaskQueue is the minimal common instruction-queue surface.
type TaskQueue interface {
	// Enqueue publishes payload; opts may defer visibility.
	Enqueue(ctx context.Context, queue string, payload []byte, opts EnqueueOptions) error

	// Dequeue leases one message for visibilityTimeout. If not Acked within
	// the window the lease lapses and the message becomes pollable again
	// (at-least-once). Returns ErrEmpty when nothing is available.
	Dequeue(ctx context.Context, queue string, visibilityTimeout time.Duration) (Receipt, error)

	// Ack completes a message: it is removed and never redelivered.
	Ack(ctx context.Context, receipt Receipt) error

	// Nack releases a message for redelivery. Past the receive-count ceiling
	// the message moves to dead-letter instead; the reason is recorded.
	Nack(ctx context.Context, receipt Receipt, reason error) error

	// ExtendVisibility renews the lease while work continues (heartbeat).
	ExtendVisibility(ctx context.Context, receipt Receipt, d time.Duration) error

	// Depth reports the number of pending+leased (non-dead) messages.
	Depth(ctx context.Context, queue string) (int64, error)
}
