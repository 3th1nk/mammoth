package store

import (
	"context"
	"os"
	"testing"
)

// TestObservePXE exercises the PXE sighting match (MAC inside the hardware
// view's nics array, separator/case-insensitive) against PostgreSQL when
// MAMMOTH_TEST_PG_DSN points at a disposable database (`make test-pg`); it
// is skipped otherwise.
func TestObservePXE(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping machines.pxe suite")
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

	// One machine whose Redfish-collected NIC spells the MAC uppercase with
	// colons; the PXE wire sends lowercase — the match must normalize both.
	const hardware = `{"disks":[],"nics":[{"name":"eno1","mac":"52:54:00:AB:CD:EF","link_up":true}]}`
	seed := func(t *testing.T) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO credentials (id, name, type, secret_encrypted)
			VALUES ('cred_pxetest', 'cred_pxetest', 'bmc', '\x00')
			ON CONFLICT (id) DO NOTHING`); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO machines (id, bmc_address, bmc_protocol, bmc_credential_id, state, hardware)
			VALUES ('mch_pxetest', '10.99.0.99', 'redfish', 'cred_pxetest', 'ready', $1)
			ON CONFLICT (id) DO UPDATE SET hardware = EXCLUDED.hardware`, hardware); err != nil {
			t.Fatalf("seed machine: %v", err)
		}
	}

	t.Run("match normalizes mac spelling", func(t *testing.T) {
		seed(t)
		hit, err := repo.ObservePXE(ctx, "52:54:00:ab:cd:ef", "uefi-x64")
		if err != nil || !hit {
			t.Fatalf("ObservePXE = (%v, %v), want hit", hit, err)
		}
		var firmware *string
		if err := db.QueryRowContext(ctx,
			`SELECT pxe_firmware FROM machines WHERE id = 'mch_pxetest'`).Scan(&firmware); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if firmware == nil || *firmware != "uefi-x64" {
			t.Errorf("pxe_firmware = %v, want uefi-x64", firmware)
		}
	})

	t.Run("dashed mac spelling also matches", func(t *testing.T) {
		seed(t)
		hit, err := repo.ObservePXE(ctx, "52-54-00-ab-cd-ef", "bios")
		if err != nil || !hit {
			t.Fatalf("ObservePXE = (%v, %v), want hit", hit, err)
		}
	})

	t.Run("unknown mac misses", func(t *testing.T) {
		hit, err := repo.ObservePXE(ctx, "00:11:22:33:44:55", "uefi-x64")
		if err != nil || hit {
			t.Fatalf("ObservePXE = (%v, %v), want miss", hit, err)
		}
	})

	t.Run("machine without hardware view misses", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO machines (id, bmc_address, bmc_protocol, bmc_credential_id, state)
			VALUES ('mch_pxetest_nohw', '10.99.0.98', 'redfish', 'cred_pxetest', 'ready')
			ON CONFLICT (id) DO UPDATE SET hardware = NULL`); err != nil {
			t.Fatalf("seed machine: %v", err)
		}
		// A MAC no other machine claims — isolates the no-hardware machine.
		hit, err := repo.ObservePXE(ctx, "aa:bb:cc:00:11:22", "uefi-x64")
		if err != nil {
			t.Fatalf("ObservePXE: %v", err)
		}
		if hit {
			t.Error("machine without nics must not match")
		}
	})
}
