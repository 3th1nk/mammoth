package store

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestPendingRepo exercises the zero-registration sighting lifecycle against
// PostgreSQL when MAMMOTH_TEST_PG_DSN points at a disposable database
// (`make test-pg`); it is skipped otherwise.
func TestPendingRepo(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping pending_machines suite")
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
	repo := NewPendingRepo(db)
	const mac = "52:54:00:11:22:33"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM pending_machines WHERE mac = $1`, mac)
	})

	// Responder sighting creates the row; first_seen_at is pinned at birth.
	created, err := repo.TouchByMAC(ctx, mac, "uefi-x64")
	if err != nil {
		t.Fatalf("touch: %v", err)
	}
	if !created {
		t.Fatalf("first touch: created = false, want true")
	}
	first, err := repo.Get(ctx, mac)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if first.Firmware == nil || *first.Firmware != "uefi-x64" || first.Report != nil {
		t.Fatalf("after touch: %+v", first)
	}

	// A later sighting refreshes firmware and last_seen_at, keeps first_seen,
	// and must not report itself as a new sighting — the console's live feed
	// keys on the first one only (PXE retries would flood it otherwise).
	created, err = repo.TouchByMAC(ctx, mac, "bios")
	if err != nil {
		t.Fatalf("touch again: %v", err)
	}
	if created {
		t.Errorf("second touch: created = true, want false")
	}
	second, err := repo.Get(ctx, mac)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if second.FirstSeenAt != first.FirstSeenAt {
		t.Errorf("first_seen_at moved: %v → %v", first.FirstSeenAt, second.FirstSeenAt)
	}
	if second.Firmware == nil || *second.Firmware != "bios" {
		t.Errorf("firmware = %v, want bios", second.Firmware)
	}

	// The enrollment probe's report lands verbatim and does not clobber the
	// responder's firmware fact. (Comparison is semantic: PG reformats
	// jsonb — spacing changes, content does not.)
	report := []byte(`{"disks":[{"name":"sda","size":"480G"}]}`)
	if err := repo.SaveReport(ctx, mac, report); err != nil {
		t.Fatalf("save report: %v", err)
	}
	third, err := repo.Get(ctx, mac)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got, want any
	if err := json.Unmarshal(third.Report, &got); err != nil {
		t.Fatalf("stored report not JSON: %v", err)
	}
	if err := json.Unmarshal(report, &want); err != nil {
		t.Fatalf("test report not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %s, want %s", third.Report, report)
	}
	if third.Firmware == nil || *third.Firmware != "bios" {
		t.Errorf("firmware = %v, want bios preserved", third.Firmware)
	}

	// A report for a never-seen MAC creates the row (probe evidence counts).
	const mac2 = "52:54:00:44:55:66"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM pending_machines WHERE mac = $1`, mac2)
	})
	if err := repo.SaveReport(ctx, mac2, report); err != nil {
		t.Fatalf("save report new mac: %v", err)
	}
	if _, err := repo.Get(ctx, mac2); err != nil {
		t.Fatalf("get after report: %v", err)
	}

	// List carries both; unknown MAC is ErrNotFound.
	all, err := repo.List(ctx)
	if err != nil || len(all) < 2 {
		t.Fatalf("list = %d items, %v; want ≥2", len(all), err)
	}
	if _, err := repo.Get(ctx, "00:00:00:00:00:01"); err != ErrNotFound {
		t.Errorf("unknown mac err = %v, want ErrNotFound", err)
	}

	// Claim path: delete consumes the row; deleting again is a no-op.
	if err := repo.Delete(ctx, mac); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, mac); err != ErrNotFound {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, mac); err != nil {
		t.Errorf("idempotent delete: %v", err)
	}
}
