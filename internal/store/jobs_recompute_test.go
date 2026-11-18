package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRecomputeJobZeroTasksSettles: a job whose tasks vanished out-of-band
// (machine delete cascades tasks.machine_id away) recomputes to a terminal
// state, NOT back to 'pending' — the cancel handler recomputes exactly then,
// and the old pending==total branch read 0==0 and re-pinned the job to
// 'pending' forever: cancel could never settle it (real-hardware: three
// zombie 排队中 jobs on the office-PC engine, 2026-11-18).
func TestRecomputeJobZeroTasksSettles(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping jobs recompute suite")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	jobs := NewJobRepo(db)
	job := NewID("tst_rj_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
	})
	// The zombie shape verbatim: submitted with an empty machine list.
	err = jobs.CreateJobWithTasks(context.Background(), CreateJobInput{
		Job: &Job{ID: job, Type: "install"},
	})
	if err != nil {
		t.Fatalf("create zero-task job: %v", err)
	}
	if err := jobs.RecomputeJob(context.Background(), job); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	j, err := jobs.GetJob(context.Background(), job)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.State != "canceled" {
		t.Fatalf("zero-task job recomputed to %q, want canceled", j.State)
	}
	if j.FinishedAt == nil || time.Time(*j.FinishedAt).IsZero() {
		t.Fatalf("settled job should carry finished_at: %+v", j.FinishedAt)
	}
}
