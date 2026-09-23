package queue

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/3th1nk/mammoth/internal/store"
)

func testPGDSN() string { return os.Getenv("MAMMOTH_TEST_PG_DSN") }

func openTestPG(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Schema via store.Migrate, not a hand-copied DDL: a copy drifts from
	// migration 00001, and a bare CREATE TABLE racing store's goose run (the
	// two test binaries start concurrently under `go test ./...`) is the
	// "relation already exists" CI failure. Migrate's advisory lock
	// serializes the two migrators.
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func truncateQueueTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`TRUNCATE queue_messages`); err != nil {
		t.Logf("cleanup: %v", err)
	}
}
