package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// The hardware health gate (docs/04-install-spec.md §5.6): block intercepts
// only on an explicit failed reading in the latest layout snapshot; no
// snapshot, no health data, or a pass installs as usual.
// PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestRejectUnhealthyMachines(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping health gate suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	machine := store.NewID("tst_gate_m_")
	cred := store.NewID("tst_gate_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, $2, 'bmc', '\x00'::bytea)`, cred, "gate-test-"+cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, $2, 'fake', $3, 'ready')`, machine, "fake://gate-"+machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	save := func(disks string) {
		raw := fmt.Sprintf(`{"captured_at":"2026-09-24T00:00:00Z","source":"ramdisk","disks":[%s]}`, disks)
		if _, err := db.Exec(`INSERT INTO layout_snapshots (machine_id, source, content) VALUES ($1, 'ramdisk', $2)`,
			machine, raw); err != nil {
			t.Fatalf("seed layout: %v", err)
		}
	}

	s := &Server{Deps{Machines: store.NewMachineRepo(db)}}

	// No snapshot: nothing to gate on.
	if err := s.rejectUnhealthyMachines(ctx, []string{machine}); err != nil {
		t.Fatalf("machine without snapshot must pass: %v", err)
	}

	save(`{"device":"nvme0n1","health":"pass"}`)
	if err := s.rejectUnhealthyMachines(ctx, []string{machine}); err != nil {
		t.Fatalf("healthy disk must pass: %v", err)
	}

	save(`{"device":"sda","serial":"GIM_BROKEN","health":"fail"}`)
	err = s.rejectUnhealthyMachines(ctx, []string{machine})
	ve, ok := err.(*validationError)
	if !ok {
		t.Fatalf("failed disk: err = %v, want *validationError", err)
	}
	if ve.status != http.StatusUnprocessableEntity || ve.Code() != "HEALTH_GATE_FAILED" {
		t.Fatalf("status/code = %d/%q, want 422/HEALTH_GATE_FAILED", ve.status, ve.Code())
	}
	if !strings.Contains(ve.Error(), "GIM_BROKEN") {
		t.Errorf("error must name the failing serial: %v", err)
	}

	// Only an explicit fail intercepts: absence of health data never does.
	save(`{"device":"sdb"}`)
	if err := s.rejectUnhealthyMachines(ctx, []string{machine}); err != nil {
		t.Fatalf("disk without health data must pass: %v", err)
	}
}
