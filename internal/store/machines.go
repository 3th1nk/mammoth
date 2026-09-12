package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MachineRepo stores registered machines.
type MachineRepo struct{ db *sql.DB }

func NewMachineRepo(db *sql.DB) *MachineRepo { return &MachineRepo{db: db} }

func (r *MachineRepo) Create(ctx context.Context, m *Machine) error {
	labels, _ := json.Marshal(m.Labels)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, ssh_credential_id, ssh_address)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.ID, labels, m.BMCAddress, m.BMCProtocol, m.BMCCredentialID, m.SSHCredentialID, m.SSHAddress)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: bmc address %q already registered", ErrConflict, m.BMCAddress)
	}
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: referenced credential does not exist", ErrNotFound)
	}
	return err
}

func (r *MachineRepo) Get(ctx context.Context, id string) (*Machine, error) {
	row := r.db.QueryRowContext(ctx, machineSelect+` WHERE id = $1`, id)
	return scanMachine(row)
}

// ListFilter narrows machine listings (docs/03-api.md §4: state + labels
// equality, cursor pagination).
type ListFilter struct {
	State    string
	Labels   []string // "k=v" pairs
	PageSize int
	Cursor   *Cursor
	Order    string // asc | desc
}

// Cursor is an opaque keyset continuation token over (created_at, id).
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

func (r *MachineRepo) List(ctx context.Context, f ListFilter) ([]*Machine, *Cursor, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.State != "" {
		args = append(args, f.State)
		where = append(where, fmt.Sprintf("state = $%d", len(args)))
	}
	for _, kv := range f.Labels {
		k, v, _ := strings.Cut(kv, "=")
		args = append(args, json.RawMessage(fmt.Sprintf(`{"%s":"%s"}`, k, v)))
		where = append(where, fmt.Sprintf("labels @> $%d", len(args)))
	}
	if f.Cursor != nil {
		args = append(args, f.Cursor.CreatedAt, f.Cursor.ID)
		op := "<"
		if f.Order == "asc" {
			op = ">"
		}
		where = append(where, fmt.Sprintf("(created_at, id) %s ($%d, $%d)", op, len(args)-1, len(args)))
	}
	order := "desc"
	if f.Order == "asc" {
		order = "asc"
	}
	limit := f.PageSize
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit+1)

	rows, err := r.db.QueryContext(ctx, machineSelect+
		" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY created_at "+order+", id "+order+
		fmt.Sprintf(" LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var items []*Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var next *Cursor
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return items, next, nil
}

// Update mutates the mutable fields: labels, bmc address/protocol/credential,
// ssh credential (docs/03-api.md §2 PATCH semantics).
func (r *MachineRepo) Update(ctx context.Context, m *Machine) error {
	labels, _ := json.Marshal(m.Labels)
	res, err := r.db.ExecContext(ctx, `
		UPDATE machines SET
		  labels = $2, bmc_address = $3, bmc_protocol = $4,
		  bmc_credential_id = $5, ssh_credential_id = $6, ssh_address = $7, updated_at = now()
		WHERE id = $1`,
		m.ID, labels, m.BMCAddress, m.BMCProtocol, m.BMCCredentialID, m.SSHCredentialID, m.SSHAddress)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: bmc address %q already registered", ErrConflict, m.BMCAddress)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *MachineRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM machines WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ProbeResult carries discovery backfill for a machine.
type ProbeResult struct {
	Vendor          *string
	Model           *string
	SerialNumber    *string
	FirmwareVersion *string
	Hardware        json.RawMessage
	PowerState      string
	State           string
	LastError       *ErrorInfo
}

// UpdateProbeResult backfills probe results — the discover flow writes here.
// Absent identity fields keep their previous value (COALESCE semantics).
func (r *MachineRepo) UpdateProbeResult(ctx context.Context, id string, p ProbeResult) error {
	var hw any
	if len(p.Hardware) > 0 {
		hw = []byte(p.Hardware)
	}
	var le any
	if p.LastError != nil {
		b, _ := json.Marshal(p.LastError)
		le = b
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE machines SET
		  vendor = COALESCE($2, vendor),
		  model = COALESCE($3, model),
		  serial_number = COALESCE($4, serial_number),
		  firmware_version = COALESCE($5, firmware_version),
		  hardware = COALESCE($6, hardware),
		  power_state = $7,
		  state = $8,
		  last_error = $9,
		  updated_at = now()
		WHERE id = $1`,
		id, p.Vendor, p.Model, p.SerialNumber, p.FirmwareVersion, hw,
		p.PowerState, p.State, le)
	return err
}

// SetLastError records a classified failure on the machine.
func (r *MachineRepo) SetError(ctx context.Context, id string, state string, lastErr *ErrorInfo) error {
	var le any
	if lastErr != nil {
		b, _ := json.Marshal(lastErr)
		le = b
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE machines SET state = $2, last_error = $3, updated_at = now() WHERE id = $1`,
		id, state, le)
	return err
}

// SetPowerState persists an observed/assumed power state.
func (r *MachineRepo) SetPowerState(ctx context.Context, id, powerState string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE machines SET power_state = $2, updated_at = now() WHERE id = $1`, id, powerState)
	return err
}

// LatestLayout returns the newest layout snapshot content (snapshots are
// immutable, append-only — docs/08-data-model.md iron rule 2).
func (r *MachineRepo) LatestLayout(ctx context.Context, machineID string) (json.RawMessage, time.Time, error) {
	var content []byte
	var captured time.Time
	err := r.db.QueryRowContext(ctx, `
		SELECT content, captured_at FROM layout_snapshots
		WHERE machine_id = $1 ORDER BY captured_at DESC, id DESC LIMIT 1`, machineID).
		Scan(&content, &captured)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return json.RawMessage(content), captured, nil
}

// LatestLayoutBySource is LatestLayout constrained to one capture source —
// the ramdisk discover flow waits on it (the probe report endpoint writes
// the snapshot; the waiting task polls for one newer than its baseline).
func (r *MachineRepo) LatestLayoutBySource(ctx context.Context, machineID, source string) (json.RawMessage, time.Time, error) {
	var content []byte
	var captured time.Time
	err := r.db.QueryRowContext(ctx, `
		SELECT content, captured_at FROM layout_snapshots
		WHERE machine_id = $1 AND source = $2
		ORDER BY captured_at DESC, id DESC LIMIT 1`, machineID, source).
		Scan(&content, &captured)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return json.RawMessage(content), captured, nil
}

const machineSelect = `
	SELECT id, labels, bmc_address, bmc_protocol, bmc_credential_id, ssh_credential_id, ssh_address,
	       vendor, model, serial_number, firmware_version, hardware,
	       power_state, state, last_error, created_at, updated_at
	FROM machines`

type rowScanner interface{ Scan(dest ...any) error }

func scanMachine(row rowScanner) (*Machine, error) {
	var m Machine
	var labels []byte
	var sshCred, vendor, model, serial, firmware sql.NullString
	var hardware, lastError []byte
	var sshAddr sql.NullString
	err := row.Scan(
		&m.ID, &labels, &m.BMCAddress, &m.BMCProtocol, &m.BMCCredentialID, &sshCred, &sshAddr,
		&vendor, &model, &serial, &firmware, &hardware,
		&m.PowerState, &m.State, &lastError, &m.CreatedAt, &m.UpdatedAt,
	)
	if err == nil {
		m.SSHAddress = sshAddr.String
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(labels, &m.Labels)
	m.SSHCredentialID = nullStrPtr(sshCred)
	ptr := func(s sql.NullString) *string {
		if !s.Valid {
			return nil
		}
		v := s.String
		return &v
	}
	m.Vendor, m.Model, m.SerialNumber, m.FirmwareVersion = ptr(vendor), ptr(model), ptr(serial), ptr(firmware)
	if len(hardware) > 0 {
		m.Hardware = json.RawMessage(hardware)
	}
	if len(lastError) > 0 {
		var ei ErrorInfo
		if json.Unmarshal(lastError, &ei) == nil {
			m.LastError = &ei
		}
	}
	return &m, nil
}

func nullStrPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// SetState moves the machine lifecycle state (registering → discovering →
// ready | error); the discover flow writes through here.
func (r *MachineRepo) SetState(ctx context.Context, id, state string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE machines SET state = $2, updated_at = now() WHERE id = $1`, id, state)
	return err
}

// SaveLayout appends an immutable layout snapshot and prunes history to the
// retention window in the same transaction (docs/08-data-model.md: snapshots
// are append-only; docs/05-inventory.md §5: newest N versions per machine).
func (r *MachineRepo) SaveLayout(ctx context.Context, machineID, source string, content json.RawMessage, keep int) (int64, error) {
	if keep <= 0 {
		keep = 10
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO layout_snapshots (machine_id, source, content, captured_at)
		VALUES ($1, $2, $3, now())
		RETURNING id`, machineID, source, content).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM layout_snapshots
		WHERE machine_id = $1 AND id NOT IN (
		  SELECT id FROM layout_snapshots WHERE machine_id = $1
		  ORDER BY captured_at DESC, id DESC LIMIT $2
		)`, machineID, keep); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// SetHardware replaces the stored hardware view (used by in-band link
// refresh, which merges NIC facts into the redfish-collected view).
func (r *MachineRepo) SetHardware(ctx context.Context, id string, hardware json.RawMessage) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE machines SET hardware = $2, updated_at = now() WHERE id = $1`, id, hardware)
	return err
}
