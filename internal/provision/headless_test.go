package provision

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/store"
)

// The headless boot stage (docs/13-headless-design.md §4), regression-guarded
// against PostgreSQL when MAMMOTH_TEST_PG_DSN points at a disposable
// database (`make test-pg`); skipped otherwise.
//
// The contract under test: arm(none) works from EVIDENCE — a PXE sighting
// of the machine after the arm watermark — with WoL and the manual
// power-on warning as the paths toward it, and the task timeout as the cap.

func headlessFixture(t *testing.T, dsn string) (*Executor, string, string, string, string) {
	t.Helper()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	machine, job, task := store.NewID("tst_hd_m_"), store.NewID("tst_hd_j_"), store.NewID("tst_hd_t_")
	mac := "52:54:00:he:ad:01"
	token := "tok-hd-" + task[len(task)-8:]
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM pending_machines WHERE mac = $1`, mac)
	})

	// A protocol=none machine: no BMC credential at all (NULL) — the shape
	// migration 00011 legalized. The hardware view carries the NIC MAC the
	// way claim fixed it from the enrollment report: it is what ObservePXE
	// (and therefore the boot-evidence path) matches on.
	hw, _ := json.Marshal(map[string]any{
		"nics": []map[string]any{{"name": "eno0", "mac": mac, "link_up": true}},
	})
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, state, hardware)
		VALUES ($1, '{}'::jsonb, $2, 'none', 'ready', $3::jsonb)`, machine, "none://"+mac, hw); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, spec_resolved, state)
		VALUES ($1, 'install', '{}'::jsonb, '{"image":{"distro":"ubuntu24","source":"file:///x.iso"}}'::jsonb, 'running')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	ictx := installTaskContext{
		Token: token, BootStrategy: "pxe",
		Netboot: &netbootRecord{Token: token, MACs: []string{mac}},
	}
	raw, _ := json.Marshal(ictx)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'running', 3, $4::jsonb)`, task, job, machine, raw); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	e := &Executor{
		Jobs:        store.NewJobRepo(db),
		Machines:    store.NewMachineRepo(db),
		Events:      store.NewEventRepo(db),
		Netboot:     store.NewNetbootRepo(db),
		Pending:     store.NewPendingRepo(db),
		BootTreeDir: t.TempDir(),
		// Fresh machines have no reachable OS: the in-band reboot is a
		// no-op miss (no ssh address/credential on the seeded row).
	}
	return e, machine, task, mac, token
}

func headlessSession(e *Executor, machine, task, jobID, mac, token string, timeoutSecs int) *bootSession {
	return &bootSession{
		Task: &store.Task{ID: task, MachineID: machine, FlowName: "install"},
		Job:  &store.Job{ID: jobID, Policy: store.Policy{TaskTimeoutSeconds: timeoutSecs}},
		Ictx: &installTaskContext{Token: token, BootStrategy: "pxe",
			Netboot: &netbootRecord{MACs: []string{mac}}},
		Spec: &installSpecView{},
	}
}

// Immediate evidence: the machine was sighted on the wire at arm time (the
// builtin-mode path — ObservePXE has already written the observation), so
// arm returns at the first tick with booted_at recorded.
func TestHeadlessArmImmediateEvidence(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping headless boot suite")
	}
	e, machine, task, mac, token := headlessFixture(t, dsn)
	if _, err := e.Machines.ObservePXE(context.Background(), mac, "uefi-x64"); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	waked := []string{}
	e.WakeOnLAN = func(m string) error { waked = append(waked, m); return nil }
	e.evidenceTick = 50 * time.Millisecond
	ctx := context.Background()

	b := headlessSession(e, machine, task, "", mac, token, 300)
	if err := e.armHeadless(ctx, b); err != nil {
		t.Fatalf("armHeadless: %v", err)
	}
	if len(waked) != 1 || waked[0] != mac {
		t.Errorf("WakeOnLAN calls = %v, want [%s]", waked, mac)
	}
	ft, err := e.Jobs.GetTask(ctx, task)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	var ictx2 installTaskContext
	if json.Unmarshal(ft.Context, &ictx2) != nil || ictx2.Install == nil ||
		ictx2.Install.BootedAt == nil {
		t.Fatalf("booted_at not recorded in task context: %s", ft.Context)
	}
}

// The manual fallback: no evidence inside the window → the warning event
// lands and the stage keeps waiting; a later sighting (the operator powered
// the machine on) completes the arm.
func TestHeadlessArmManualFallbackSelfHeals(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping headless boot suite")
	}
	e, machine, task, mac, token := headlessFixture(t, dsn)
	e.evidenceTick = 50 * time.Millisecond
	e.HeadlessBootWait = 300 * time.Millisecond // evidence window (spec-tunable in production)
	ctx := context.Background()

	// The "operator" powers the machine on ~600ms in: a proxy sighting
	// lands in the pending ledger (external mode's evidence path).
	go func() {
		time.Sleep(600 * time.Millisecond)
		_, _ = e.Pending.TouchByMAC(ctx, mac, "uefi-x64")
	}()

	b := headlessSession(e, machine, task, "", mac, token, 300)
	if err := e.armHeadless(ctx, b); err != nil {
		t.Fatalf("armHeadless must self-heal on the later sighting: %v", err)
	}
	// The manual warning fired before the evidence arrived.
	items, err := e.Events.List(ctx, store.EventFilter{ResourceType: "task", ResourceID: task})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	found := false
	for _, ev := range items {
		if ev.Type == "task.boot_manual_required" {
			found = true
		}
	}
	if !found {
		t.Fatalf("task.boot_manual_required missing from events (%d events)", len(items))
	}
}

// The cap: no evidence at all → BOOT_EVIDENCE_TIMEOUT at the task timeout,
// retryable (the operator may power the machine before the retry).
func TestHeadlessArmTimeout(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping headless boot suite")
	}
	e, machine, task, mac, token := headlessFixture(t, dsn)
	e.evidenceTick = 50 * time.Millisecond
	e.HeadlessBootWait = 200 * time.Millisecond
	ctx := context.Background()

	b := headlessSession(e, machine, task, "", mac, token, 1) // cap = 1s
	err := e.armHeadless(ctx, b)
	if err == nil {
		t.Fatal("want BOOT_EVIDENCE_TIMEOUT")
	}
	if !strings.Contains(err.Error(), "BOOT_EVIDENCE_TIMEOUT") {
		t.Fatalf("err = %v, want BOOT_EVIDENCE_TIMEOUT", err)
	}
}
