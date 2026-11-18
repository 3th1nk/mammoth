package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// TestDeleteMachineGatesOnUnfinishedTasks: tasks.machine_id is ON DELETE
// CASCADE, so a bare delete would silently erase queued/running work and
// strand the job as a zero-task zombie cancel can never settle (the
// office-PC round's three zombie 排队中 jobs, 2026-11-18). The delete must
// answer 409 JOB_MACHINE_BUSY while an install task lives, then succeed
// once the job is settled.
func TestDeleteMachineGatesOnUnfinishedTasks(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping machine delete gate suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	job, task := store.NewID("tst_dg_j_"), store.NewID("tst_dg_t_")
	machine, cred := store.NewID("tst_dg_m_"), store.NewID("tst_dg_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_logs WHERE task_id = $1`, task)
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'delete-gate-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://dg', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'pending')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'pending', 0, '{}'::jsonb)`, task, job, machine); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	s := &Server{Deps{
		Jobs:     store.NewJobRepo(db),
		Machines: store.NewMachineRepo(db),
		Events:   store.NewEventRepo(db),
	}}
	ctx := context.Background()

	// Busy: the queued install task must block the delete with the same
	// 409 shape the submit-side gate uses.
	if _, err = s.DeleteMachine(ctx, gen.DeleteMachineRequestObject{Id: gen.MachineId(machine)}); err == nil {
		t.Fatal("delete with a queued install task must be rejected")
	}
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "JOB_MACHINE_BUSY" {
		t.Fatalf("busy rejection should carry JOB_MACHINE_BUSY, got: %v", err)
	}
	if !strings.Contains(err.Error(), job) {
		t.Fatalf("busy rejection should name the blocking job, got: %v", err)
	}
	if _, ferr := s.Jobs.GetTask(ctx, task); ferr != nil {
		t.Fatalf("task must survive the rejected delete: %v", ferr)
	}

	// Settle the job (control-plane cancel, the sanctioned write), then the
	// delete goes through.
	if _, err := db.Exec(`UPDATE tasks SET state='canceled', finished_at=now() WHERE id=$1`, task); err != nil {
		t.Fatalf("settle task: %v", err)
	}
	if _, err := db.Exec(`UPDATE jobs SET state='canceled', finished_at=now() WHERE id=$1`, job); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if _, err := s.DeleteMachine(ctx, gen.DeleteMachineRequestObject{Id: gen.MachineId(machine)}); err != nil {
		t.Fatalf("delete after settle: %v", err)
	}
	if _, ferr := s.Machines.Get(ctx, machine); ferr == nil {
		t.Fatal("machine should be gone after the settled delete")
	}
}
