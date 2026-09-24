package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// PendingRepo owns the pending_machines table (docs/09-roadmap.md,
// zero-registration entry): machines that announced themselves over PXE but
// are not registered yet. Two feeders write here — the PXE responder
// (firmware observations for unknown MACs, via MachineRepo.ObservePXE's
// miss path) and the enrollment probe (the /sys report). Claim — promoting a
// sighting to a registered machine — is a follow-up operation.
type PendingRepo struct{ db *sql.DB }

func NewPendingRepo(db *sql.DB) *PendingRepo { return &PendingRepo{db: db} }

// PendingMachine is one zero-registration sighting.
type PendingMachine struct {
	MAC         string          `json:"mac"`
	Firmware    *string         `json:"firmware,omitempty"`
	Report      json.RawMessage `json:"report,omitempty"`
	FirstSeenAt time.Time       `json:"first_seen_at"`
	LastSeenAt  time.Time       `json:"last_seen_at"`
}

// TouchByMAC records a responder sighting for an unknown MAC: the firmware
// architecture it announced (option 93 label). The first DISCOVER creates
// the row; later sightings refresh firmware and last_seen_at only —
// first_seen_at stays the birth of the relationship. Returns whether this
// call created the row, so callers can fire a first-sighting event without
// flooding the stream on every PXE retry.
func (r *PendingRepo) TouchByMAC(ctx context.Context, mac, firmware string) (bool, error) {
	// (xmax = 0) is the canonical insert-or-update discriminator: a row
	// fresh from this INSERT has no locker. Worst case after exotic vacuum
	// activity is a spurious true — a duplicate event, never lost data.
	var created bool
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO pending_machines (mac, firmware) VALUES ($1, $2)
		ON CONFLICT (mac) DO UPDATE
		SET firmware = EXCLUDED.firmware, last_seen_at = now()
		RETURNING (xmax = 0)`, mac, firmware).Scan(&created)
	return created, err
}

// SaveReport records an enrollment probe's /sys scan for the MAC it booted
// from, creating the row when the responder never saw it. Verbatim by
// design — the report shape is the probe's contract, not the store's.
func (r *PendingRepo) SaveReport(ctx context.Context, mac string, report json.RawMessage) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO pending_machines (mac, report) VALUES ($1, $2)
		ON CONFLICT (mac) DO UPDATE
		SET report = EXCLUDED.report, last_seen_at = now()`,
		mac, report)
	return err
}

// List returns every sighting, oldest relationship first.
func (r *PendingRepo) List(ctx context.Context) ([]*PendingMachine, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT mac, firmware, report, first_seen_at, last_seen_at
		FROM pending_machines ORDER BY first_seen_at, mac`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*PendingMachine
	for rows.Next() {
		m, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

// Get returns one sighting by normalized MAC.
func (r *PendingRepo) Get(ctx context.Context, mac string) (*PendingMachine, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT mac, firmware, report, first_seen_at, last_seen_at
		FROM pending_machines WHERE mac = $1`, mac)
	m, err := scanPending(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// Delete removes a claimed sighting. Idempotent by design — the claim flow
// migrates the row's data into the machine first, so a lost race here (two
// claims of one MAC) is caught by the bmc_address unique constraint on the
// machine side, and the second delete is simply a no-op.
func (r *PendingRepo) Delete(ctx context.Context, mac string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM pending_machines WHERE mac = $1`, mac)
	return err
}

func scanPending(sc interface{ Scan(...any) error }) (*PendingMachine, error) {
	m := &PendingMachine{}
	var report []byte
	if err := sc.Scan(&m.MAC, &m.Firmware, &report, &m.FirstSeenAt, &m.LastSeenAt); err != nil {
		return nil, err
	}
	if len(report) > 0 {
		m.Report = json.RawMessage(report)
	}
	return m, nil
}
