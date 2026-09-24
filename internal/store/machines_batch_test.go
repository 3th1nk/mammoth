package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestMachineBatchUpdateLabels exercises the all-or-nothing batch label
// update (docs/04 §A5): remove deletes by key, add upserts, one unknown id
// rejects the whole batch, and the result comes back in request order.
// PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestMachineBatchUpdateLabels(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping batch-labels suite")
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

	cred := NewID("tst_bl_c_")
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'batch-labels-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred) })

	ids := []string{NewID("tst_bl_a_"), NewID("tst_bl_b_"), NewID("tst_bl_c2_")}
	seeds := []string{`{"env":"dev","tmp":"yes"}`, `{"rack":"B1"}`, `{}`}
	for i, id := range ids {
		if _, err := db.Exec(`
			INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
			VALUES ($1, $2::jsonb, $3, 'fake', $4, 'ready')`,
			id, seeds[i], "10.9.9."+string(rune('1'+i)), cred); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, id) })
	}

	// add + remove across machines with different starting labels.
	out, err := repo.BatchUpdateLabels(ctx, ids[0:2],
		map[string]string{"env": "prod"}, []string{"tmp"})
	if err != nil {
		t.Fatalf("batch update: %v", err)
	}
	if out[0].ID != ids[0] || out[1].ID != ids[1] {
		t.Errorf("result not in request order: %s, %s", out[0].ID, out[1].ID)
	}
	if out[0].Labels["env"] != "prod" {
		t.Errorf("a: env upsert failed: %v", out[0].Labels)
	}
	if _, has := out[0].Labels["tmp"]; has {
		t.Errorf("a: tmp not removed: %v", out[0].Labels)
	}
	if out[1].Labels["env"] != "prod" || out[1].Labels["rack"] != "B1" {
		t.Errorf("b: add must not clobber other labels: %v", out[1].Labels)
	}
	// remove of an absent key is a no-op, not an error.
	out, err = repo.BatchUpdateLabels(ctx, ids[2:3], nil, []string{"ghost"})
	if err != nil {
		t.Fatalf("absent-key remove: %v", err)
	}
	if len(out[0].Labels) != 0 {
		t.Errorf("c: labels mutated by no-op: %v", out[0].Labels)
	}

	// All-or-nothing: one unknown id rolls everything back.
	before, err := repo.Get(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.BatchUpdateLabels(ctx, []string{ids[0], "mch_missing"},
		map[string]string{"env": "staging"}, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
	after, err := repo.Get(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if after.Labels["env"] != "prod" {
		t.Errorf("rejected batch must leave no trace: %v → %v", before.Labels, after.Labels)
	}
}
