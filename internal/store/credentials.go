package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// CredentialRepo stores encrypted BMC/SSH credentials.
type CredentialRepo struct{ db *sql.DB }

func NewCredentialRepo(db *sql.DB) *CredentialRepo { return &CredentialRepo{db: db} }

func (r *CredentialRepo) Create(ctx context.Context, c *Credential) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, $2, $3, $4)`,
		c.ID, c.Name, c.Type, c.SecretEncrypted)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: credential name %q already exists", ErrConflict, c.Name)
	}
	return err
}

// List returns every credential's metadata, newest first. Secrets are
// never part of the result — this feeds the console's credential picker.
func (r *CredentialRepo) List(ctx context.Context) ([]*Credential, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, type, created_at, updated_at
		FROM credentials ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*Credential
	for rows.Next() {
		c := &Credential{}
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func (r *CredentialRepo) Get(ctx context.Context, id string) (*Credential, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, name, type, secret_encrypted, created_at, updated_at
		FROM credentials WHERE id = $1`, id)
	return scanCredential(row)
}

func (r *CredentialRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = $1`, id)
	if err != nil {
		// Machines still referencing this credential trip the FK.
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: credential still referenced by a machine", ErrConflict)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanCredential(row *sql.Row) (*Credential, error) {
	var c Credential
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.SecretEncrypted, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
