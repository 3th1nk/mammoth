package store

import (
	"context"
	"os"
	"testing"
)

// TestMarkStageNoop: the configure_raid pass-through flag persists alongside
// the normal stage lifecycle — StartStage/AdvanceStage keep running (retry
// re-enters the same row), and the flag lands on exactly one stage, leaving
// siblings untouched (docs/06-install-pipeline.md §2 six-stage contract).
func TestMarkStageNoop(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping stage noop suite")
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
	cred := NewID("tst_np_c_")
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'noop-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred) })

	machine := NewID("tst_np_m_")
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}', 'fake://noop', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine) })

	job := NewID("tst_np_j_")
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, created_by)
		VALUES ($1, 'install', '{"type":"install"}', 'test')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job) })

	task := NewID("tst_np_t_")
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state)
		VALUES ($1, $2, $3, 'install', 'running')`, task, job, machine); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// The install six-stage contract verbatim (StageNames lives in provision;
	// store tests spell the contract out).
	for seq, name := range []string{"verify_layout", "configure_raid", "prepare_media", "boot", "install_os", "verify_ready"} {
		if _, err := db.Exec(`
			INSERT INTO task_stages (task_id, seq, name)
			VALUES ($1, $2, $3)`, task, seq, name); err != nil {
			t.Fatalf("seed stage %s: %v", name, err)
		}
	}

	jobs := NewJobRepo(db)

	// Baseline: nothing flagged.
	stages, err := jobs.Stages(ctx, task)
	if err != nil {
		t.Fatalf("stages: %v", err)
	}
	for _, s := range stages {
		if s.Noop {
			t.Fatalf("fresh task has noop stage %s", s.Name)
		}
	}

	if err := jobs.StartStage(ctx, task, 1); err != nil {
		t.Fatalf("start stage: %v", err)
	}
	if err := jobs.MarkStageNoop(ctx, task, 1); err != nil {
		t.Fatalf("mark noop: %v", err)
	}
	if err := jobs.AdvanceStage(ctx, task, 1); err != nil {
		t.Fatalf("advance stage: %v", err)
	}

	stages, err = jobs.Stages(ctx, task)
	if err != nil {
		t.Fatalf("stages: %v", err)
	}
	for _, s := range stages {
		want := s.Seq == 1
		if s.Noop != want {
			t.Fatalf("stage %s noop=%v, want %v", s.Name, s.Noop, want)
		}
	}
	if stages[1].State != "succeeded" {
		t.Fatalf("noop stage advanced to %q, want succeeded", stages[1].State)
	}
}
