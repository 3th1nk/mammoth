package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// The machine-busy gate (runbook known defect: same-machine resubmits piled
// up pending reinstall tasks; the workaround was a manual batch cancel
// before every re-run). PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestRejectBusyInstallMachines(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping machine-busy gate suite")
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
	machine, job, task := store.NewID("tst_busy_m_"), store.NewID("tst_busy_j_"), store.NewID("tst_busy_t_")
	cred := store.NewID("tst_busy_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'busy-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://busy', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'running')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'pending', 0, '{}'::jsonb)`, task, job, machine); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	s := &Server{Deps{Jobs: store.NewJobRepo(db)}}

	// Busy machine (pending install task): 409 with the task/job identity.
	err = s.rejectBusyInstallMachines(ctx, []string{machine})
	ve, ok := err.(*validationError)
	if !ok {
		t.Fatalf("busy machine: err = %v, want *validationError", err)
	}
	if ve.status != http.StatusConflict || ve.Code() != "JOB_MACHINE_BUSY" {
		t.Fatalf("status/code = %d/%q, want 409/JOB_MACHINE_BUSY", ve.status, ve.Code())
	}
	if !strings.Contains(ve.Error(), task) || !strings.Contains(ve.Error(), job) {
		t.Errorf("error must name the blocking task/job: %v", err)
	}

	// The active-state matrix: running and interrupted count as busy too.
	for _, state := range []string{"running", "interrupted"} {
		if _, err := db.Exec(`UPDATE tasks SET state = $1 WHERE id = $2`, state, task); err != nil {
			t.Fatal(err)
		}
		if err := s.rejectBusyInstallMachines(ctx, []string{machine}); err == nil {
			t.Fatalf("%s task must count as busy", state)
		}
	}

	// Terminal states free the machine: a resubmit passes.
	for _, state := range []string{"succeeded", "failed", "canceled"} {
		if _, err := db.Exec(`UPDATE tasks SET state = $1 WHERE id = $2`, state, task); err != nil {
			t.Fatal(err)
		}
		if err := s.rejectBusyInstallMachines(ctx, []string{machine}); err != nil {
			t.Fatalf("%s task must NOT block a resubmit: %v", state, err)
		}
	}

	// A discover/power task on the machine never blocks an install.
	if _, err := db.Exec(`
		UPDATE tasks SET state = 'pending', flow_name = 'discover' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	if err := s.rejectBusyInstallMachines(ctx, []string{machine}); err != nil {
		t.Fatalf("non-install flow must not block: %v", err)
	}

	// Sibling machines in the same submission stay unaffected.
	if err := s.rejectBusyInstallMachines(ctx, []string{machine, machine + "_x"}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
