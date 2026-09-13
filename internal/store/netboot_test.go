package store

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"
)

// TestNetbootRepo exercises the netboot_entries lifecycle against
// PostgreSQL when MAMMOTH_TEST_PG_DSN points at a disposable database
// (`make test-pg`); it is skipped otherwise.
func TestNetbootRepo(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping netboot_entries suite")
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
	repo := NewNetbootRepo(db)
	const mac = "52:54:00:aa:bb:cc"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM netboot_entries WHERE mac = $1`, mac)
		_, _ = db.Exec(`DELETE FROM tasks WHERE id LIKE 'tst_nb%'`)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id LIKE 'tst_nb%'`)
		_, _ = db.Exec(`DELETE FROM machines WHERE id LIKE 'tst_nb%'`)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id LIKE 'tst_nb%'`)
	})

	// A terminal-state task to drive the orphan sweep.
	machine := NewID("tst_nb_m_")
	job := NewID("tst_nb_j_")
	task := NewID("tst_nb_t_")
	cred := NewID("tst_nb_c_")
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'nb-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://nb', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'succeeded')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context, updated_at)
		VALUES ($1, $2, $3, 'install', 'succeeded', 6, '{}'::jsonb, now() - interval '2 hours')`,
		task, job, machine); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Arm, then re-arm the same MAC: the upsert must overwrite, not collide.
	e := &NetbootEntry{
		MAC: mac, TaskID: task, MachineID: machine, Token: "tok1", Kind: "install",
		Kernel: "vmlinuz", Initrd: "initrd.img", KernelArgs: "inst.ks=http://x/ks.cfg",
		Extra: map[string]string{"modloop": "modloop-lts"},
	}
	if err := repo.Upsert(ctx, e); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	e2 := &NetbootEntry{
		MAC: mac, TaskID: task, MachineID: machine, Token: "tok2", Kind: "probe",
		Kernel: "vmlinuz-lts", Initrd: "initramfs-lts",
	}
	if err := repo.Upsert(ctx, e2); err != nil {
		t.Fatalf("re-arm: %v", err)
	}

	got, err := repo.ByMAC(ctx, mac)
	if err != nil {
		t.Fatalf("by mac: %v", err)
	}
	if got.Token != "tok2" || got.Kind != "probe" || got.KernelArgs != "" {
		t.Fatalf("re-arm not applied: %+v", got)
	}
	if got.Extra["modloop"] != "" {
		t.Fatalf("stale extra survived: %v", got.Extra)
	}

	byTok, err := repo.ByToken(ctx, "tok2")
	if err != nil || byTok.MAC != mac {
		t.Fatalf("by token: %+v %v", byTok, err)
	}
	if _, err := repo.ByToken(ctx, "no-such-token"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if !entryHasFile(got, "vmlinuz-lts") || !entryHasFile(got, "initramfs-lts") {
		t.Fatalf("allowlist incomplete: %v", got.AllowlistedFiles())
	}

	// The orphan sweep only takes entries pointing at a terminal task that
	// have themselves gone stale (a fresh entry means someone is actively
	// working — it must survive). Age the entry, then sweep.
	if _, err := db.Exec(`UPDATE netboot_entries SET updated_at = now() - interval '2 hours' WHERE mac = $1`, mac); err != nil {
		t.Fatalf("age entry: %v", err)
	}
	deleted, err := repo.DeleteTerminated(ctx, time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Token != "tok2" || deleted[0].MAC != mac {
		t.Fatalf("sweep result: %+v", deleted)
	}
	if _, err := repo.ByMAC(ctx, mac); err != ErrNotFound {
		t.Fatalf("entry survived sweep: %v", err)
	}
}

func entryHasFile(e *NetbootEntry, name string) bool {
	return slices.Contains(e.AllowlistedFiles(), name)
}
