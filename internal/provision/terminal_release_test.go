package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
)

// The two real-hardware known defects from the windows agent apply rounds
// (docs/runbooks/windows-agent-apply.md), regression-guarded here against
// PostgreSQL when MAMMOTH_TEST_PG_DSN points at a disposable database
// (`make test-pg`); skipped otherwise.
//
//  1. cancel/terminal release used the CLAIM-TIME task snapshot, whose
//     context predates prepare_media's patch (token + boot_strategy) — the
//     release misrouted to virtual-media and leaked the netboot entries and
//     boot tree (the machine re-entered the old path on its next reboot).
//  2. machines stayed in "discovering" after a successful install: the
//     ramdisk probe opens the lifecycle transition but never settles it,
//     and the install flow never touches machine state.

func releaseFreshFixture(t *testing.T, dsn string) (*Executor, string, string, string, string) {
	t.Helper()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	machine, job, task := store.NewID("tst_rel_m_"), store.NewID("tst_rel_j_"), store.NewID("tst_rel_t_")
	cred := store.NewID("tst_rel_c_")
	mac := "52:54:00:re:le:01"
	token := "tok-rel-" + task[len(task)-8:]
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM netboot_entries WHERE task_id = $1`, task)
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	// The BMC credential is deliberately undecryptable ('\x00' under a test
	// master key): outOfBand degrades to ok=false at the decrypt step, which
	// is exactly the no-BMC shape verify_ready's success face must settle
	// under (the boot-order restore no-ops).
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'rel-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://rel', 'fake', $2, 'discovering')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, spec_resolved, state)
		VALUES ($1, 'install', '{}'::jsonb, '{"image":{"distro":"rocky9","source":"file:///x.iso"}}'::jsonb, 'running')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// The FRESH context: what prepare_media patched into the record after
	// arming the payload (the claim-time snapshot the runner holds carries
	// none of this). The install record rides the same shape the completion
	// report handler persists — verify_ready's success face reads it.
	ictx := installTaskContext{
		Token: token, BootStrategy: "pxe",
		Netboot: &netbootRecord{Token: token, MACs: []string{mac}},
		Install: &installProgress{Status: "ok", Detail: "windows setup finished"},
	}
	raw, _ := json.Marshal(ictx)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'running', 3, $4::jsonb)`, task, job, machine, raw); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	crypto, cerr := store.NewSecretCrypto("92MSQJfHiGPv1XTGmWIqUUbLPJe0sUvyUFD59jLqaA0=") // base64 of a test key
	if cerr != nil {
		t.Fatalf("crypto: %v", cerr)
	}
	e := &Executor{
		Jobs:        store.NewJobRepo(db),
		Machines:    store.NewMachineRepo(db),
		Events:      store.NewEventRepo(db),
		Credentials: store.NewCredentialRepo(db),
		Crypto:      crypto,
		Render:      render.NewRegistry(),
		Netboot:     store.NewNetbootRepo(db),
		BootTreeDir: t.TempDir(),
	}
	return e, machine, task, mac, token
}

func TestReleaseBootPayloadFresh(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping terminal release suite")
	}
	e, machine, task, mac, token := releaseFreshFixture(t, dsn)
	ctx := context.Background()

	if err := e.Netboot.Upsert(ctx, &store.NetbootEntry{
		MAC: mac, TaskID: task, MachineID: machine, Token: token, Kind: "install",
	}); err != nil {
		t.Fatalf("arm entry: %v", err)
	}
	treeDir := filepath.Join(e.BootTreeDir, token)
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The runner holds the claim-time snapshot: empty context, no token, no
	// strategy. The fresh re-read inside must still route the release to
	// the PXE cleaner (entry deleted + tree removed) — the pre-fix behavior
	// misrouted to virtual-media and leaked both.
	stale := &store.Task{ID: task, MachineID: machine, FlowName: "install", Context: []byte("{}")}
	e.releaseBootPayloadFresh(ctx, stale, "canceled")

	if _, err := e.Netboot.ByMAC(ctx, mac); err == nil {
		t.Fatal("netboot entry survived the cancel-shaped release")
	}
	if _, err := os.Stat(treeDir); !os.IsNotExist(err) {
		t.Fatal("boot tree survived the cancel-shaped release")
	}
}

func TestVerifyReadySettlesMachineState(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping terminal release suite")
	}
	e, machine, task, _, _ := releaseFreshFixture(t, dsn)
	ctx := context.Background()

	// verify_ready re-enters from the stored record (fresh-read discipline);
	// the fixture context already carries the completion report. No SSH
	// configured (the report is the verification surface), no BMC
	// credential (the boot-order restore no-ops) — the settle must still
	// move the machine out of the transitional state.
	job, err := e.Jobs.GetJob(ctx, jobID(t, e, task))
	if err != nil {
		t.Fatalf("fixture job: %v", err)
	}
	fresh, err := e.Jobs.GetTask(ctx, task)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := e.verifyReady(ctx, fresh, job); err != nil {
		t.Fatalf("verifyReady: %v", err)
	}
	m, err := e.Machines.Get(ctx, machine)
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if m.State != "ready" {
		t.Fatalf("machine state = %q, want ready after a verified install", m.State)
	}
}

// jobID resolves the fixture task's job (there is exactly one).
func jobID(t *testing.T, e *Executor, taskID string) string {
	t.Helper()
	task, err := e.Jobs.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	return task.JobID
}
