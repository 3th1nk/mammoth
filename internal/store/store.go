// Package store holds the persistence layer: goose SQL migrations, the
// resource repos, and the task queue. PostgreSQL is the authoritative store;
// repos here target PostgreSQL (the SQLite path is the task-queue test
// implementation in internal/store/queue).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" database/sql driver
	"github.com/pressly/goose/v3"
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

// Migrate applies pending migrations (idempotent, run at startup).
func Migrate(ctx context.Context, db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
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
