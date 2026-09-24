package store

import (
	"context"
	"os"
	"testing"
)

// Deregister cascade (migration 00010): deleting a machine takes its layout
// snapshots and task rows with it; the job shell and events stay. Regression
// for the bare-RESTRICT FK that made every machine with an auto-discovery
// task undeletable (SQLSTATE 23503 → 500). PG-gated via MAMMOTH_TEST_PG_DSN.
func TestMachineDeleteCascades(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping deregister cascade suite")
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
	repo := NewMachineRepo(db)

	cred := NewID("tst_del_c_")
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'dereg-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred) })

	id := NewID("tst_del_m_")
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}', 'fake://dereg', 'fake', $2, 'ready')`, id, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, id) })

	if _, err := db.Exec(`
		INSERT INTO layout_snapshots (machine_id, source, content)
		VALUES ($1, 'ramdisk', '{}')`, id); err != nil {
		t.Fatalf("seed layout: %v", err)
	}
	job := NewID("tst_del_j_")
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, created_by)
		VALUES ($1, 'discover', '{"type":"discover"}', 'test')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job) })
	taskID := NewID("tst_del_t_")
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state)
		VALUES ($1, $2, $3, 'discover', 'succeeded')`, taskID, job, id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_stages (task_id, seq, name)
		VALUES ($1, 0, 'verify_layout')`, taskID); err != nil {
		t.Fatalf("seed stage: %v", err)
	}

	if err := repo.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var layouts, tasks, stages int
	if err := db.QueryRow(`SELECT count(*) FROM layout_snapshots WHERE machine_id = $1`, id).Scan(&layouts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE machine_id = $1`, id).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`
		SELECT count(*) FROM task_stages s JOIN tasks t ON s.task_id = t.id WHERE t.machine_id = $1`, id).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if layouts != 0 || tasks != 0 || stages != 0 {
		t.Errorf("machine-scoped rows survived: layouts=%d tasks=%d stages=%d, want 0/0/0", layouts, tasks, stages)
	}
	var jobs int
	if err := db.QueryRow(`SELECT count(*) FROM jobs WHERE id = $1`, job).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("job shell deleted with machine, want retained")
	}
	if _, err := repo.Get(ctx, id); err == nil {
		t.Error("machine still present after delete")
	}
}
