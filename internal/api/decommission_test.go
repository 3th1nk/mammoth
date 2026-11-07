package api

// The decommission flow's submission gates and evidence face
// (docs/07-bmc.md §6.6): confirm gate, busy-gate bidirectionality, the
// retired terminal state (422 on flows, discover reinstates), and the
// persisted report. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
)

func decommissionSeed(t *testing.T, dsn string) (*Server, *sql.DB, context.Context, string, string) {
	t.Helper()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	machine := store.NewID("tst_dc_m_")
	cred := store.NewID("tst_dc_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE machine_id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id IN (SELECT job_id FROM tasks WHERE machine_id = $1)`, machine)
		_, _ = db.Exec(`DELETE FROM decommission_reports WHERE machine_id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	s := &Server{Deps{
		Machines:             store.NewMachineRepo(db),
		Events:               store.NewEventRepo(db),
		Jobs:                 store.NewJobRepo(db),
		Queue:                queue.NewPG(db, queue.Options{}),
		EraseConfirmRequired: true,
	}}
	return s, db, ctx, machine, cred
}

func TestDecommissionSubmissionGates(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping decommission gate suite")
	}
	s, db, ctx, machine, cred := decommissionSeed(t, dsn)

	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'dc-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://dc', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	submit := func(confirm *bool) error {
		_, err := s.CreateJob(ctx, gen.CreateJobRequestObject{
			Body: &gen.CreateJobJSONRequestBody{
				Type:    gen.JobType("decommission"),
				Targets: gen.JobTargets{Targets: []gen.JobTarget{{MachineId: gen.MachineId(machine)}}},
				Confirm: confirm,
			}})
		return err
	}

	// No confirm: 422, no job — the one gate.
	err := submit(nil)
	ve, ok := err.(*validationError)
	if !ok || ve.status != http.StatusUnprocessableEntity || ve.Code() != "DRIVE_ERASE_CONFIRM_REQUIRED" {
		t.Fatalf("no-confirm: want 422 DRIVE_ERASE_CONFIRM_REQUIRED, got %v", err)
	}

	// With confirm: 202, job type decommission.
	confirm := true
	resp, err := s.CreateJob(ctx, gen.CreateJobRequestObject{
		Body: &gen.CreateJobJSONRequestBody{
			Type:    gen.JobType("decommission"),
			Targets: gen.JobTargets{Targets: []gen.JobTarget{{MachineId: gen.MachineId(machine)}}},
			Confirm: &confirm,
		}})
	if err != nil {
		t.Fatalf("with confirm: %v", err)
	}
	job, ok := resp.(gen.CreateJob202JSONResponse)
	if !ok || job.Id == "" {
		t.Fatalf("job response wrong: %+v", job)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE job_id = $1`, job.Id)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job.Id)
	})

	// Busy gate: a second decommission on the same machine → 409.
	err = submit(&confirm)
	ve, ok = err.(*validationError)
	if !ok || ve.status != http.StatusConflict || ve.Code() != "JOB_MACHINE_BUSY" {
		t.Fatalf("busy: want 409 JOB_MACHINE_BUSY, got %v", err)
	}
}

func TestRetiredMachineGatesAndReport(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping decommission gate suite")
	}
	s, db, ctx, machine, _ := decommissionSeed(t, dsn)

	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, state)
		VALUES ($1, '{}'::jsonb, 'fake://dc-r', 'fake', 'ready')`, machine); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	// No report yet: {"report": null}, 200.
	resp, err := s.GetMachineDecommission(ctx, gen.GetMachineDecommissionRequestObject{Id: gen.MachineId(machine)})
	if err != nil {
		t.Fatalf("no-report read: %v", err)
	}
	view, ok := resp.(gen.GetMachineDecommission200JSONResponse)
	if !ok || view.Report != nil {
		t.Fatalf("no-report read must carry null report, got %+v", view)
	}

	// Retire the machine (the flow's terminal write, direct-seeded here).
	if _, err := db.Exec(`UPDATE machines SET state = 'retired' WHERE id = $1`, machine); err != nil {
		t.Fatal(err)
	}

	// Flows answer 422 MACHINE_RETIRED; discover is the reinstatement path
	// and is deliberately NOT gated (the probe walks state back to ready).
	confirm := true
	_, err = s.CreateJob(ctx, gen.CreateJobRequestObject{
		Body: &gen.CreateJobJSONRequestBody{
			Type:    gen.JobType("decommission"),
			Targets: gen.JobTargets{Targets: []gen.JobTarget{{MachineId: gen.MachineId(machine)}}},
			Confirm: &confirm,
		}})
	ve, ok := err.(*validationError)
	if !ok || ve.status != http.StatusUnprocessableEntity || ve.Code() != "MACHINE_RETIRED" {
		t.Fatalf("decommission on retired: want 422 MACHINE_RETIRED, got %v", err)
	}

	// The report face: persist one, read it back.
	ev := map[string]any{
		"machine": map[string]any{"id": machine, "vendor": "acme"},
		"erase":   map[string]any{"serials": []string{"S6XPN0001"}},
	}
	raw, _ := json.Marshal(ev)
	if err := s.Machines.SaveDecommissionReport(ctx, &store.DecommissionReport{
		MachineID: machine, TaskID: "t1", Content: raw,
	}); err != nil {
		t.Fatalf("save report: %v", err)
	}
	resp, err = s.GetMachineDecommission(ctx, gen.GetMachineDecommissionRequestObject{Id: gen.MachineId(machine)})
	if err != nil {
		t.Fatalf("report read: %v", err)
	}
	view, ok = resp.(gen.GetMachineDecommission200JSONResponse)
	if !ok || view.Report == nil {
		t.Fatalf("report must round-trip, got %+v", view)
	}

	// Reinstatement: the state walks back — a retired machine becomes
	// flow-eligible again.
	if _, err := db.Exec(`UPDATE machines SET state = 'ready' WHERE id = $1`, machine); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateJob(ctx, gen.CreateJobRequestObject{
		Body: &gen.CreateJobJSONRequestBody{
			Type:    gen.JobType("decommission"),
			Targets: gen.JobTargets{Targets: []gen.JobTarget{{MachineId: gen.MachineId(machine)}}},
			Confirm: &confirm,
		}})
	if err != nil {
		t.Fatalf("reinstated machine must be flow-eligible: %v", err)
	}
}
