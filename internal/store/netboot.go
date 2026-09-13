package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// NetbootRepo owns the netboot_entries table (docs/08-data-model.md): the
// registry of machines armed for network boot. The runner Upserts during
// prepare_media and deletes on release; the machine-face script endpoint
// resolves PXE clients by MAC. Rows are keyed by MAC (one pending boot per
// machine — a BMC boots one NIC at a time in practice, and the boot tree is
// identical for every NIC of the machine).
type NetbootRepo struct{ db *sql.DB }

func NewNetbootRepo(db *sql.DB) *NetbootRepo { return &NetbootRepo{db: db} }

// NetbootEntry is one pending network boot registration.
type NetbootEntry struct {
	ID         int64             `json:"id"`
	MAC        string            `json:"mac"`
	TaskID     string            `json:"task_id"`
	MachineID  string            `json:"machine_id"`
	Token      string            `json:"-"`
	Kind       string            `json:"kind"` // install | probe
	Kernel     string            `json:"kernel"`
	Initrd     string            `json:"initrd"`
	KernelArgs string            `json:"kernel_args,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// AllowlistedFiles lists the file names servable for this entry: kernel,
// initrd, and any auxiliary files in extra (the HTTP file handler's
// allowlist — nothing else leaves the boot tree directory).
func (e *NetbootEntry) AllowlistedFiles() []string {
	names := make([]string, 0, 2+len(e.Extra))
	for _, n := range []string{e.Kernel, e.Initrd} {
		if n != "" {
			names = append(names, n)
		}
	}
	for _, n := range e.Extra {
		if n != "" {
			names = append(names, n)
		}
	}
	return names
}

// Upsert arms (or re-arms) the machine: prepare_media retries overwrite the
// row instead of colliding with UNIQUE(mac).
func (r *NetbootRepo) Upsert(ctx context.Context, e *NetbootEntry) error {
	var extra any
	if len(e.Extra) > 0 {
		b, err := json.Marshal(e.Extra)
		if err != nil {
			return err
		}
		extra = b
	}
	return r.db.QueryRowContext(ctx, `
		INSERT INTO netboot_entries (mac, task_id, machine_id, token, kind, kernel, initrd, kernel_args, extra)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (mac) DO UPDATE SET
			task_id = EXCLUDED.task_id,
			machine_id = EXCLUDED.machine_id,
			token = EXCLUDED.token,
			kind = EXCLUDED.kind,
			kernel = EXCLUDED.kernel,
			initrd = EXCLUDED.initrd,
			kernel_args = EXCLUDED.kernel_args,
			extra = EXCLUDED.extra,
			updated_at = now()
		RETURNING id, created_at, updated_at`,
		e.MAC, e.TaskID, e.MachineID, e.Token, e.Kind,
		e.Kernel, e.Initrd, e.KernelArgs, extra,
	).Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
}

const netbootCols = `id, mac, task_id, machine_id, token, kind, kernel, initrd, kernel_args, extra, created_at, updated_at`

func scanNetboot(row interface{ Scan(...any) error }) (*NetbootEntry, error) {
	var e NetbootEntry
	var extra []byte
	if err := row.Scan(&e.ID, &e.MAC, &e.TaskID, &e.MachineID, &e.Token, &e.Kind,
		&e.Kernel, &e.Initrd, &e.KernelArgs, &extra, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		_ = json.Unmarshal(extra, &e.Extra)
	}
	return &e, nil
}

// ByMAC resolves an incoming PXE client. ErrNotFound when the MAC has no
// pending boot — the caller answers the client with the exit fallback.
func (r *NetbootRepo) ByMAC(ctx context.Context, mac string) (*NetbootEntry, error) {
	e, err := scanNetboot(r.db.QueryRowContext(ctx,
		`SELECT `+netbootCols+` FROM netboot_entries WHERE mac = $1`, mac))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// ByToken resolves a boot tree request (the token names the directory).
func (r *NetbootRepo) ByToken(ctx context.Context, token string) (*NetbootEntry, error) {
	e, err := scanNetboot(r.db.QueryRowContext(ctx,
		`SELECT `+netbootCols+` FROM netboot_entries WHERE token = $1`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// DeleteByMAC disarms a machine; false when nothing was pending.
func (r *NetbootRepo) DeleteByMAC(ctx context.Context, mac string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM netboot_entries WHERE mac = $1`, mac)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteByTask is the cancel/retry sweep: drop every entry a task owns.
func (r *NetbootRepo) DeleteByTask(ctx context.Context, taskID string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM netboot_entries WHERE task_id = $1`, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeletedTree is an entry removed by the orphan sweep; the caller removes
// its boot tree directory (the store knows nothing of the filesystem).
type DeletedTree struct {
	Token string
	MAC   string
}

// DeleteTerminated removes entries whose owning task reached a terminal
// state at least maxAge ago (reaper orphan sweep). Only terminal states —
// a pending/running task's entry IS its live boot intent.
func (r *NetbootRepo) DeleteTerminated(ctx context.Context, maxAge time.Duration) ([]DeletedTree, error) {
	rows, err := r.db.QueryContext(ctx, `
		DELETE FROM netboot_entries e
		USING tasks t
		WHERE e.task_id = t.id
		  AND t.state IN ('succeeded', 'failed', 'canceled')
		  AND e.updated_at < now() - make_interval(secs => $1)
		RETURNING e.token, e.mac`, maxAge.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeletedTree
	for rows.Next() {
		var d DeletedTree
		if err := rows.Scan(&d.Token, &d.MAC); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
