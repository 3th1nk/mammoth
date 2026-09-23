// Package store holds the persistence layer: goose SQL migrations, the
// resource repos, and the task queue. PostgreSQL is the only store —
// migrations, repos, and the table queue share one database (docs/08,
// docs/10-tech-stack.md D2/D3).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open connects via database/sql (pgx stdlib driver) so migrations and repos
// share one handle.
func Open(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return db, nil
}

// Migrate applies pending migrations (idempotent, run at startup). The
// session-level advisory lock serializes concurrent migrators sharing one
// database — `go test` starts one test binary per package against the same
// PG, and multiple replicas boot against one DB too; goose's version table
// makes re-runs no-ops but cannot guard two first-time runs racing on an
// empty database (the loser dies on "relation already exists").
func Migrate(ctx context.Context, db *sql.DB) error {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations,
		goose.WithLogger(goose.NopLogger()),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// NewID generates a prefixed opaque identifier (`mch_`, `job_`, …). Internal
// serials never leave the process (docs/03-api.md §1).
func NewID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("store: id entropy: %v", err)) // crypto/rand failure is not recoverable
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
