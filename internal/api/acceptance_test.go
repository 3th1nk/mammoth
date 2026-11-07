package api

// The acceptance report read face (docs/06-install-pipeline.md §5):
// SaveAcceptance prunes to a rolling window, the endpoint returns the
// latest report or {"report": null} — a persisted read, no BMC call, no
// 404 ambiguity. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).
import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

func TestMachineAcceptanceEndpoint(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping acceptance endpoint suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	machine := store.NewID("tst_acc_m_")
	cred := store.NewID("tst_acc_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'acc-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://acc', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	machines := store.NewMachineRepo(db)
	s := &Server{Deps{Machines: machines}}

	// No report yet: {"report": null}, 200 — never a 404.
	resp, err := s.GetMachineAcceptance(ctx, gen.GetMachineAcceptanceRequestObject{Id: gen.MachineId(machine)})
	if err != nil {
		t.Fatalf("no-report read: %v", err)
	}
	view, ok := resp.(gen.GetMachineAcceptance200JSONResponse)
	if !ok {
		t.Fatalf("no-report read: unexpected response %T", resp)
	}
	if view.Report != nil {
		t.Fatalf("no-report read must carry null report, got %+v", view.Report)
	}

	// Unknown machine: the resource, not the report, is missing — the
	// strict handler's error func turns store.ErrNotFound into the 404.
	_, err = s.GetMachineAcceptance(ctx, gen.GetMachineAcceptanceRequestObject{Id: gen.MachineId(machine + "_x")})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown machine: want store.ErrNotFound (wrapped 404), got %v", err)
	}

	// Save two reports; the endpoint returns the newest, history prunes.
	sys := json.RawMessage(`{"hostname":"node-1","kernel":"6.12.4","os":"Ubuntu 24.04"}`)
	first := &store.AcceptanceReport{MachineID: machine, TaskID: "task-1", Verdict: "degraded", System: sys,
		Checks: []store.AcceptanceCheck{{Name: "system_facts", Status: "ok"}}}
	if err := machines.SaveAcceptance(ctx, first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	second := &store.AcceptanceReport{MachineID: machine, TaskID: "task-2", Verdict: "pass", System: sys,
		Checks: []store.AcceptanceCheck{
			{Name: "system_facts", Status: "ok"},
			{Name: "hostname", Status: "ok", Detail: "node-1"},
		}}
	if err := machines.SaveAcceptance(ctx, second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	if second.ID == "" || first.ID == second.ID {
		t.Fatalf("report ids must be minted (first=%q second=%q)", first.ID, second.ID)
	}

	resp, err = s.GetMachineAcceptance(ctx, gen.GetMachineAcceptanceRequestObject{Id: gen.MachineId(machine)})
	if err != nil {
		t.Fatalf("latest read: %v", err)
	}
	view, ok = resp.(gen.GetMachineAcceptance200JSONResponse)
	if !ok {
		t.Fatalf("latest read: unexpected response %T", resp)
	}
	if view.Report == nil || view.Report.TaskId != "task-2" || view.Report.Verdict != "pass" {
		t.Fatalf("latest report wrong: %+v", view.Report)
	}
	if len(view.Report.Checks) != 2 || view.Report.Checks[1].Name != "hostname" {
		t.Fatalf("checks wrong: %+v", view.Report.Checks)
	}
	if view.Report.System == nil || (*view.Report.System)["hostname"] != "node-1" {
		t.Fatalf("system facts wrong: %+v", view.Report.System)
	}

	// Rolling window (LIMIT 10, layout_snapshots' discipline): three saves
	// stay under the window — the prune is the same proven shape SaveLayout
	// runs, exercised here only for the count staying consistent.
	third := &store.AcceptanceReport{MachineID: machine, TaskID: "task-3", Verdict: "pass"}
	if err := machines.SaveAcceptance(ctx, third); err != nil {
		t.Fatalf("save third: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM acceptance_reports WHERE machine_id = $1`, machine).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("rows under the window: %d, want 3", n)
	}
}
