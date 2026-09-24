package api

import (
	"context"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// The machine's in-flight view (machines/{id}/current-tasks): every
// unfinished task regardless of flow — the console's busy badge and
// reinstall precheck. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestListMachineCurrentTasks(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping current-tasks suite")
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
	machine := store.NewID("tst_cur_m_")
	cred := store.NewID("tst_cur_c_")
	installJob, installTask := store.NewID("tst_cur_ij_"), store.NewID("tst_cur_it_")
	powerJob, powerTask := store.NewID("tst_cur_pj_"), store.NewID("tst_cur_pt_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = ANY($1)`, []string{installTask, powerTask})
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = ANY($1)`, []string{installJob, powerJob})
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'current-tasks-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://current-tasks', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'running')`, installJob); err != nil {
		t.Fatalf("seed install job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'power', '{}'::jsonb, 'running')`, powerJob); err != nil {
		t.Fatalf("seed power job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'pending', 0, '{}'::jsonb)`, installTask, installJob, machine); err != nil {
		t.Fatalf("seed install task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'power', 'running', 0, '{}'::jsonb)`, powerTask, powerJob, machine); err != nil {
		t.Fatalf("seed power task: %v", err)
	}

	s := &Server{Deps{Machines: store.NewMachineRepo(db), Jobs: store.NewJobRepo(db)}}
	req := gen.ListMachineCurrentTasksRequestObject{Id: gen.MachineId(machine)}

	// Both flows appear, oldest (install) first — power may legitimately run
	// alongside an install, so the view is per-flow, not single-task.
	resp, err := s.ListMachineCurrentTasks(ctx, req)
	if err != nil {
		t.Fatalf("current tasks: %v", err)
	}
	page, ok := resp.(gen.ListMachineCurrentTasks200JSONResponse)
	if !ok {
		t.Fatalf("response type %T", resp)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	first := page.Items[0]
	if first.FlowName != "install" || first.State != "pending" ||
		string(first.TaskId) != installTask || string(first.JobId) != installJob {
		t.Errorf("first ref = %+v, want install task %s", first, installTask)
	}
	if page.Items[1].FlowName != "power" {
		t.Errorf("second ref flow = %q, want power", page.Items[1].FlowName)
	}

	// A terminal install task drops out of the view; the running one stays.
	if _, err := db.Exec(`UPDATE tasks SET state = 'canceled' WHERE id = $1`, installTask); err != nil {
		t.Fatal(err)
	}
	resp, err = s.ListMachineCurrentTasks(ctx, req)
	if err != nil {
		t.Fatalf("current tasks after cancel: %v", err)
	}
	page = resp.(gen.ListMachineCurrentTasks200JSONResponse)
	if len(page.Items) != 1 || page.Items[0].FlowName != "power" {
		t.Errorf("after cancel = %+v, want only power", page.Items)
	}

	// All-terminal: empty items, not 404 — "free" is a normal answer.
	if _, err := db.Exec(`UPDATE tasks SET state = 'succeeded' WHERE id = $1`, powerTask); err != nil {
		t.Fatal(err)
	}
	resp, err = s.ListMachineCurrentTasks(ctx, req)
	if err != nil {
		t.Fatalf("current tasks all-terminal: %v", err)
	}
	page = resp.(gen.ListMachineCurrentTasks200JSONResponse)
	if len(page.Items) != 0 {
		t.Errorf("free machine items = %+v, want empty", page.Items)
	}

	// Unknown machine is 404 (ErrNotFound via the shared error mapper).
	_, err = s.ListMachineCurrentTasks(ctx, gen.ListMachineCurrentTasksRequestObject{Id: gen.MachineId(store.NewID("tst_cur_none_"))})
	if err != store.ErrNotFound {
		t.Errorf("unknown machine err = %v, want ErrNotFound", err)
	}
}
