package store

// BIOS baseline profiles (migration 00014, docs/07-bmc.md §6.1): named
// declarative attribute sets — the drift-inspection reference and the batch
// apply's payload source. Pure data: the vendor's own JSON types ride
// through, validation is the caller's (the API layer rejects non-object
// bodies and empty sets via the set_bios_attributes contract's own rules).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// BiosProfileRepo holds the profile CRUD.
type BiosProfileRepo struct {
	db *sql.DB
}

// NewBiosProfileRepo builds the repo.
func NewBiosProfileRepo(db *sql.DB) *BiosProfileRepo {
	return &BiosProfileRepo{db: db}
}

// Create persists a new profile (name unique — a duplicate is ErrConflict's
// business via the UNIQUE constraint; the API layer maps it).
func (r *BiosProfileRepo) Create(ctx context.Context, p *BiosProfile) error {
	if p.ID == "" {
		p.ID = NewID("bp_")
	}
	raw, err := json.Marshal(p.Attributes)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO bios_profiles (id, name, attributes)
		VALUES ($1, $2, $3)`, p.ID, p.Name, raw)
	return err
}

// Get fetches one profile (ErrNotFound when absent).
func (r *BiosProfileRepo) Get(ctx context.Context, id string) (*BiosProfile, error) {
	return r.scanOne(r.db.QueryRowContext(ctx, `
		SELECT id, name, attributes, created_at, updated_at
		FROM bios_profiles WHERE id = $1`, id))
}

// List returns every profile, newest first (fleet profiles are few; no
// pagination until reality asks for it).
func (r *BiosProfileRepo) List(ctx context.Context) ([]*BiosProfile, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, attributes, created_at, updated_at
		FROM bios_profiles ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BiosProfile
	for rows.Next() {
		p, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes a profile. machines.bios_profile_id is ON DELETE SET NULL
// (migration 00014): assignments clear, nothing dangles.
func (r *BiosProfileRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM bios_profiles WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AssignedMachines counts machines assigned to the profile (the apply
// fanout's default target set and the drift-scope view).
func (r *BiosProfileRepo) AssignedMachines(ctx context.Context, profileID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM machines WHERE bios_profile_id = $1 ORDER BY id`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *BiosProfileRepo) scanOne(row *sql.Row) (*BiosProfile, error) {
	p, err := r.scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (r *BiosProfileRepo) scanRow(row rowScanner) (*BiosProfile, error) {
	var p BiosProfile
	var attrs []byte
	if err := row.Scan(&p.ID, &p.Name, &attrs, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(attrs, &p.Attributes)
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	return &p, nil
}
