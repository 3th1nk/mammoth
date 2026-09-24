package store

import (
	"context"
	"os"
	"testing"
)

// TestCredentialList exercises the metadata-only listing the console's
// credential picker consumes (newest first, no secret material).
// PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestCredentialList(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping credential list suite")
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
	repo := NewCredentialRepo(db)
	first := &Credential{ID: NewID("tst_list_c1_"), Name: "list-test-1", Type: "bmc", SecretEncrypted: []byte("\x00")}
	second := &Credential{ID: NewID("tst_list_c2_"), Name: "list-test-2", Type: "ssh", SecretEncrypted: []byte("\x00")}
	for _, c := range []*Credential{first, second} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
		t.Cleanup(func() { _ = repo.Delete(ctx, c.ID) })
	}

	items, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []string
	for _, c := range items {
		if c.Name == first.Name || c.Name == second.Name {
			got = append(got, c.Name)
		}
	}
	if len(got) != 2 {
		t.Fatalf("list = %v, want both seeded credentials", got)
	}
	// newest first
	if got[0] != second.Name {
		t.Errorf("first item = %q, want newest %q", got[0], second.Name)
	}
}
