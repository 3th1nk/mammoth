package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestTaskLogRepo exercises the task_logs repo against PostgreSQL when
// MAMMOTH_TEST_PG_DSN points at a disposable database (`make test-pg`); it
// is skipped otherwise. The disposable DB carries the full schema via
// Migrate (goose is idempotent).
func TestTaskLogRepo(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping task_logs suite")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	repo := NewTaskLogRepo(db)
	taskID := NewID("tst_tlog_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_logs WHERE task_id = $1`, taskID)
	})

	repo.AppendTaskLog(ctx, taskID, "INFO", "prepare_media", "media assembled",
		map[string]any{"iso": "boot-tok1.iso"})
	repo.AppendTaskLog(ctx, taskID, "WARN", "", "retry scheduled", nil)
	repo.AppendTaskLog(ctx, taskID, "ERROR", "verify_ready", "probe failed",
		map[string]any{"code": "CREDENTIAL_AUTH_FAILED", "err": "auth failed"})

	logs, err := repo.List(ctx, TaskLogFilter{TaskID: taskID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("want 3 lines, got %d", len(logs))
	}
	if logs[0].Level != "INFO" || logs[0].Stage != "prepare_media" {
		t.Fatalf("line 0 mismatch: %+v", logs[0])
	}
	if logs[2].Level != "ERROR" {
		t.Fatalf("write order broken: line 2 is %s", logs[2].Level)
	}
	var attrs map[string]any
	if err := json.Unmarshal(logs[2].Attrs, &attrs); err != nil || attrs["code"] != "CREDENTIAL_AUTH_FAILED" {
		t.Fatalf("attrs roundtrip: %v %v", attrs, err)
	}
	// nil attrs must stay absent, not {}
	if logs[1].Attrs != nil {
		t.Fatalf("nil attrs should scan as NULL, got %s", logs[1].Attrs)
	}

	// Cursor: page after the first id returns exactly the tail.
	page2, err := repo.List(ctx, TaskLogFilter{TaskID: taskID, AfterID: logs[0].ID})
	if err != nil {
		t.Fatalf("list cursor: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != logs[1].ID {
		t.Fatalf("cursor page mismatch: %+v", page2)
	}
	// Other tasks' lines never leak into the filter.
	other, err := repo.List(ctx, TaskLogFilter{TaskID: NewID("tst_none_")})
	if err != nil || len(other) != 0 {
		t.Fatalf("foreign task leaked: %v %v", other, err)
	}

	// Retention: a negative TTL (ts < now+1h) expires everything.
	n, err := repo.ExpireTaskLogs(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n < 3 {
		t.Fatalf("want >=3 expired, got %d", n)
	}
	after, err := repo.List(ctx, TaskLogFilter{TaskID: taskID})
	if err != nil || len(after) != 0 {
		t.Fatalf("post-expiry list: %v %v", after, err)
	}
}
