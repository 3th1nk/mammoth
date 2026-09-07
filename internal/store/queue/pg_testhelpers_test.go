package queue

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
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

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS queue_messages (
		    id               bigserial PRIMARY KEY,
		    queue            text NOT NULL,
		    payload          bytea NOT NULL,
		    visible_at       timestamptz NOT NULL DEFAULT now(),
		    lease_token      text,
		    lease_expired_at timestamptz,
		    delivery_count   int NOT NULL DEFAULT 0,
		    dead             boolean NOT NULL DEFAULT false,
		    dead_reason      text,
		    enqueued_at      timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return db
}

func truncateQueueTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`TRUNCATE queue_messages`); err != nil {
		t.Logf("cleanup: %v", err)
	}
}
