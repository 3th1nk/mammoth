package store

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestMachineListQ exercises the fuzzy search filter of MachineRepo.List —
// literal (escaped) substring match over the identity columns and labels.
// PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
func TestMachineListQ(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping machine q= suite")
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

	cred := NewID("tst_q_c_")
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'q-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred) })

	seed := []struct {
		id, bmc, serial, vendor, model, labels string
	}{
		{"tst_q_a_", "10.0.0.1", "SN-QA-001", "Dell Inc.", "PowerEdge R750", `{"env":"prod"}`},
		{"tst_q_b_", "10.0.0.2", "SN-QA-002", "Inspur", "NF5280M6", `{"env":"dev"}`},
		{"tst_q_c_", "fake://c", "OTHER-999", "ACME", "Box X", `{"rack":"A3"}`},
	}
	for _, s := range seed {
		if _, err := db.Exec(`
			INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, serial_number, vendor, model, state)
			VALUES ($1, $2::jsonb, $3, 'fake', $4, $5, $6, $7, 'ready')`,
			s.id, s.labels, s.bmc, cred, s.serial, s.vendor, s.model); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM machines WHERE id LIKE $1`, s.id+"%")
		})
	}

	cases := []struct {
		name string
		q    string
		want []string
	}{
		{"serial prefix", "SN-QA-0", []string{"a", "b"}},
		{"serial case-insensitive", "sn-qa-001", []string{"a"}},
		{"bmc address", "10.0.0.2", []string{"b"}},
		{"vendor", "dell", []string{"a"}},
		{"model", "NF52", []string{"b"}},
		{"label value", "prod", []string{"a"}},
		{"label key", "rack", []string{"c"}},
		{"machine id", "tst_q_a", []string{"a"}},
		{"no hit", "does-not-exist", nil},
		// LIKE metacharacters match literally: a bare % must not degrade
		// into "match everything".
		{"percent literal", "%", nil},
		{"underscore literal", "SN_QA", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, _, err := repo.List(ctx, ListFilter{Q: tc.q, PageSize: 200})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var got []string
			for _, m := range items {
				for i, s := range seed {
					if strings.HasPrefix(m.ID, s.id) {
						got = append(got, string(rune('a'+i)))
					}
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("q=%q hit %v, want %v", tc.q, got, tc.want)
			}
		})
	}

	// q combines with the other filters.
	items, _, err := repo.List(ctx, ListFilter{Q: "SN-QA", State: "ready", Labels: []string{"env=dev"}, PageSize: 200})
	if err != nil {
		t.Fatalf("combined list: %v", err)
	}
	if len(items) != 1 || items[0].ID[:len("tst_q_b_")] != "tst_q_b_" {
		t.Errorf("combined filter = %d items, want exactly b", len(items))
	}

	// order_by=updated_at actually orders by the updated column (the keyset
	// keys on the requested sort column, not always created_at).
	if _, err := db.Exec(`UPDATE machines SET updated_at = now() WHERE id LIKE 'tst_q_b_%'`); err != nil {
		t.Fatalf("touch b: %v", err)
	}
	items, _, err = repo.List(ctx, ListFilter{Q: "SN-QA", OrderBy: "updated_at", PageSize: 200})
	if err != nil || len(items) != 2 {
		t.Fatalf("updated_at list: %d items, %v", len(items), err)
	}
	if items[0].ID[:len("tst_q_b_")] != "tst_q_b_" {
		t.Errorf("updated_at desc first = %s, want b (just updated)", items[0].ID)
	}
}
