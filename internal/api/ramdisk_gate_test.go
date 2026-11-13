package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// The ramdisk-discover hazard gate (docs/05-inventory.md §4): probeRamdisk
// power-cycles the machine and its compensation powers it off — against a
// machine still serving traffic that is an outage, not an inventory. The API
// half answers at submission: busy first (an in-flight owning flow would see
// its armed PXE entries clobbered), then the power-state confirm gate (only
// a machine known off passes without the explicit confirm flag; unknown
// means never reliably read — the conservative side is asking).

func assertRamdiskConfirmReject(t *testing.T, err error) {
	t.Helper()
	ve, ok := err.(*validationError)
	if !ok {
		t.Fatalf("err = %v, want *validationError", err)
	}
	if ve.status != http.StatusUnprocessableEntity || ve.Code() != "RAMDISK_CONFIRM_REQUIRED" {
		t.Fatalf("status/code = %d/%q, want 422/RAMDISK_CONFIRM_REQUIRED", ve.status, ve.Code())
	}
	if !strings.Contains(ve.Error(), "confirm") {
		t.Errorf("error must name the confirm escape hatch: %v", err)
	}
}

// The power-state matrix of the pure half: on and unknown reject, off passes
// — the confirm escape hatch re-submits clean.
func TestValidateRamdiskConfirmPowerStateMatrix(t *testing.T) {
	action := json.RawMessage(`{"type":"discover","probe":"ramdisk"}`)
	cases := []struct {
		name       string
		powerState string
		wantReject bool
	}{
		{"on requires confirm", "on", true},
		{"unknown requires confirm", "unknown", true},
		{"off passes without confirm", "off", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &store.Machine{ID: "mch_ramdisk_test", PowerState: tc.powerState}
			err := validateRamdiskConfirm(m, action)
			if !tc.wantReject {
				if err != nil {
					t.Fatalf("off machine must pass without confirm: %v", err)
				}
				return
			}
			assertRamdiskConfirmReject(t, err)
		})
	}
}

// The explicit confirm flag is the designed escape hatch at every power
// state — the operator has stated the machine is theirs to take over.
func TestValidateRamdiskConfirmExplicitConfirmPasses(t *testing.T) {
	action := json.RawMessage(`{"type":"discover","probe":"ramdisk","confirm":true}`)
	for _, ps := range []string{"on", "unknown", "off"} {
		m := &store.Machine{ID: "mch_ramdisk_test", PowerState: ps}
		if err := validateRamdiskConfirm(m, action); err != nil {
			t.Fatalf("confirm=true must pass power state %q: %v", ps, err)
		}
	}
}

// Non-ramdisk actions never reach the gates: the registration auto-enqueue
// (probe=auto) and the redfish/inband_ssh flavors are read-only, installs
// carry no action at all. The wrappers must early-exit before any store
// access (nil repos here would panic if the gate ran).
func TestValidateRamdiskGatesSkipNonRamdisk(t *testing.T) {
	for _, raw := range []string{
		`{"type":"discover","probe":"auto"}`,
		`{"type":"discover","probe":"redfish"}`,
		`{"type":"discover","probe":"inband_ssh"}`,
		`{"type":"power_on"}`,
	} {
		is, err := isRamdiskDiscover(json.RawMessage(raw))
		if err != nil || is {
			t.Fatalf("%s: is=%v err=%v, want false/nil", raw, is, err)
		}
	}
	if is, err := isRamdiskDiscover(nil); err != nil || is {
		t.Fatalf("nil action (install submissions): is=%v err=%v, want false/nil", is, err)
	}
	s := &Server{}
	m := &store.Machine{ID: "mch_ramdisk_test", PowerState: "on"}
	if err := s.validateRamdiskAction(context.Background(), m, json.RawMessage(`{"type":"discover","probe":"auto"}`)); err != nil {
		t.Fatalf("auto discover must skip the gate: %v", err)
	}
	if err := s.validateRamdiskBatch(context.Background(), []string{"mch_ramdisk_test"}, json.RawMessage(`{"type":"power_on"}`)); err != nil {
		t.Fatalf("power action must skip the gate: %v", err)
	}
}

// The single-machine wiring against the real store (PG-gated via
// MAMMOTH_TEST_PG_DSN, `make test-pg`): busy first, then the power-state
// confirm — the same order the submission handler answers in.
func TestValidateRamdiskActionGates(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping ramdisk gate suite")
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
	machine, job, task := store.NewID("tst_ramdisk_m_"), store.NewID("tst_ramdisk_j_"), store.NewID("tst_ramdisk_t_")
	cred := store.NewID("tst_ramdisk_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'ramdisk-gate-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state, power_state)
		VALUES ($1, '{}'::jsonb, 'fake://ramdisk', 'fake', $2, 'ready', 'on')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'running')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'pending', 0, '{}'::jsonb)`, task, job, machine); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	s := &Server{Deps{Jobs: store.NewJobRepo(db), Machines: store.NewMachineRepo(db)}}
	action := json.RawMessage(`{"type":"discover","probe":"ramdisk"}`)
	fetch := func() *store.Machine {
		t.Helper()
		m, err := s.Machines.Get(ctx, machine)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// An in-flight install task answers 409 before the power-state question:
	// the probe would clobber the install's armed PXE entries.
	err = s.validateRamdiskAction(ctx, fetch(), action)
	ve, ok := err.(*validationError)
	if !ok || ve.status != http.StatusConflict || ve.Code() != "JOB_MACHINE_BUSY" {
		t.Fatalf("busy machine: err = %v, want 409/JOB_MACHINE_BUSY", err)
	}

	// Terminal install frees the machine for the power-state question: an
	// "on" machine rejects with the confirm escape hatch named.
	if _, err := db.Exec(`UPDATE tasks SET state = 'canceled' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	assertRamdiskConfirmReject(t, s.validateRamdiskAction(ctx, fetch(), action))

	// The probe's own compensation records the power-off, so a re-submission
	// against a machine mammoth itself powered down passes clean.
	if _, err := db.Exec(`UPDATE machines SET power_state = 'off' WHERE id = $1`, machine); err != nil {
		t.Fatal(err)
	}
	if err := s.validateRamdiskAction(ctx, fetch(), action); err != nil {
		t.Fatalf("off machine must pass without confirm: %v", err)
	}
}

// The batch wiring (POST /jobs) against the real store (PG-gated): same
// confirm gate, naming the machine; the confirm-flag batch passes.
func TestValidateRamdiskBatchGates(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping ramdisk gate suite")
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
	machine := store.NewID("tst_ramdisk_b_")
	cred := store.NewID("tst_ramdisk_bc_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'ramdisk-batch-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state, power_state)
		VALUES ($1, '{}'::jsonb, 'fake://ramdisk-b', 'fake', $2, 'ready', 'on')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	s := &Server{Deps{Jobs: store.NewJobRepo(db), Machines: store.NewMachineRepo(db)}}

	assertRamdiskConfirmReject(t, s.validateRamdiskBatch(ctx, []string{machine},
		json.RawMessage(`{"type":"discover","probe":"ramdisk"}`)))
	if err := s.validateRamdiskBatch(ctx, []string{machine},
		json.RawMessage(`{"type":"discover","probe":"ramdisk","confirm":true}`)); err != nil {
		t.Fatalf("confirm batch must pass: %v", err)
	}
}
