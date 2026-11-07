package api

// The BIOS baseline profile surface (docs/07-bmc.md §6.2): CRUD, machine
// assignment, live drift inspection, and the apply fanout under the
// two-stage contract. PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/bmc/fake"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
)

func TestBiosProfilesLifecycle(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping bios-profile suite")
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
	machine := store.NewID("tst_bp_m_")
	cred := store.NewID("tst_bp_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})

	crypto, cerr := store.NewSecretCrypto("92MSQJfHiGPv1XTGmWIqUUbLPJe0sUvyUFD59jLqaA0=") // base64 of a test key
	if cerr != nil {
		t.Fatalf("crypto: %v", cerr)
	}
	credRepo := store.NewCredentialRepo(db)
	secret, _ := json.Marshal(map[string]string{"username": "root", "password": "whatever"})
	enc, eerr := crypto.Encrypt(secret)
	if eerr != nil {
		t.Fatalf("encrypt secret: %v", eerr)
	}
	if err := credRepo.Create(ctx, &store.Credential{
		ID: cred, Name: "bp-test", Type: "bmc", SecretEncrypted: enc,
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://bp', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	s := &Server{Deps{
		Credentials:         credRepo,
		Crypto:              crypto,
		Machines:            store.NewMachineRepo(db),
		BiosProfiles:        store.NewBiosProfileRepo(db),
		Events:              store.NewEventRepo(db),
		Jobs:                store.NewJobRepo(db),
		Queue:               queue.NewPG(db, queue.Options{}),
		BMC:                 bmc.NewRegistry(nil, 0),
		BiosConfirmRequired: true,
	}}
	fakeDriver := fake.New()
	s.BMC.Register(fakeDriver)

	// The baseline carries one matched entry (BootMode=UEFI on the fake's
	// default table), one drifted entry (SrIovEnable true vs live false),
	// and one name the live table has never heard of.
	attrs := map[string]any{"BootMode": "UEFI", "SrIovEnable": true, "Nonexistent": true}
	name := "baseline-" + machine[7:] // per-run unique (machine ids are random)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM bios_profiles WHERE name = $1`, name)
		_, _ = db.Exec(`DELETE FROM bios_profiles WHERE name = $2`, name, "empty-"+machine[7:])
	})

	// Create: 201, minted id.
	resp, err := s.CreateBiosProfile(ctx, gen.CreateBiosProfileRequestObject{
		Body: &gen.BiosProfileCreate{Name: name, Attributes: attrs}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created, ok := resp.(gen.CreateBiosProfile201JSONResponse)
	if !ok || created.Id == "" || created.Attributes["BootMode"] != "UEFI" {
		t.Fatalf("create response wrong: %+v", created)
	}

	// Empty attributes: 422 — an empty baseline is a typo wearing a name.
	_, err = s.CreateBiosProfile(ctx, gen.CreateBiosProfileRequestObject{
		Body: &gen.BiosProfileCreate{Name: "empty-" + machine[7:], Attributes: map[string]any{}}})
	if verr, ok := err.(*validationError); !ok || verr.status != http.StatusUnprocessableEntity {
		t.Fatalf("empty attributes: want 422, got %v", err)
	}

	// Duplicate name: 409.
	_, err = s.CreateBiosProfile(ctx, gen.CreateBiosProfileRequestObject{
		Body: &gen.BiosProfileCreate{Name: name, Attributes: attrs}})
	ve, ok := err.(*validationError)
	if !ok || ve.status != http.StatusConflict || ve.Code() != "BIOS_PROFILE_NAME_TAKEN" {
		t.Fatalf("duplicate name: want 409 BIOS_PROFILE_NAME_TAKEN, got %v", err)
	}

	// Unknown profile assignment: 422.
	missing := "bp_nope"
	_, err = s.SetMachineBiosBaseline(ctx, gen.SetMachineBiosBaselineRequestObject{
		Id: gen.MachineId(machine), Body: &gen.BiosBaselineSet{ProfileId: &missing}})
	if verr, ok := err.(*validationError); !ok || verr.Code() != "SCHEMA_UNKNOWN_PROFILE" {
		t.Fatalf("unknown profile: want SCHEMA_UNKNOWN_PROFILE, got %v", err)
	}

	// Assign → machine carries it; drift read compares live vs baseline.
	pid := created.Id
	if _, err := s.SetMachineBiosBaseline(ctx, gen.SetMachineBiosBaselineRequestObject{
		Id: gen.MachineId(machine), Body: &gen.BiosBaselineSet{ProfileId: &pid}}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	m, _ := s.Machines.Get(ctx, machine)
	if m.BIOSProfileID == nil || *m.BIOSProfileID != pid {
		t.Fatalf("assignment missing: %+v", m.BIOSProfileID)
	}

	dresp, err := s.GetMachineBiosDrift(ctx, gen.GetMachineBiosDriftRequestObject{Id: gen.MachineId(machine)})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	dview, ok := dresp.(gen.GetMachineBiosDrift200JSONResponse)
	if !ok || dview.Drift == nil {
		t.Fatalf("drift view wrong: %+v", dview)
	}
	if dview.Drift.Matched != 1 {
		t.Errorf("BootMode equal → matched = 1, got %d", dview.Drift.Matched)
	}
	if dview.Drift.Drifted == nil || (*dview.Drift.Drifted)["SrIovEnable"] == nil {
		t.Errorf("SrIovEnable true-vs-false must drift: %+v", dview.Drift.Drifted)
	}
	if dview.Drift.Unknown == nil || len(*dview.Drift.Unknown) != 1 || (*dview.Drift.Unknown)[0] != "Nonexistent" {
		t.Errorf("baseline's fake name must surface as unknown: %+v", dview.Drift.Unknown)
	}

	// No baseline → {"drift": null}.
	if _, err := s.SetMachineBiosBaseline(ctx, gen.SetMachineBiosBaselineRequestObject{
		Id: gen.MachineId(machine), Body: &gen.BiosBaselineSet{ProfileId: str("")}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	dresp, err = s.GetMachineBiosDrift(ctx, gen.GetMachineBiosDriftRequestObject{Id: gen.MachineId(machine)})
	if err != nil {
		t.Fatalf("drift after clear: %v", err)
	}
	dview, ok = dresp.(gen.GetMachineBiosDrift200JSONResponse)
	if !ok || dview.Drift != nil {
		t.Fatalf("cleared machine must answer null drift, got %+v", dview)
	}

	// Apply: confirm gate → 422; with confirm → 202 job carrying the
	// profile's attributes.
	if _, err := s.ApplyBiosProfile(ctx, gen.ApplyBiosProfileRequestObject{
		Id: gen.ProfileId(pid), Body: &gen.BiosProfileApply{MachineIds: &[]string{machine}}}); err == nil {
		t.Fatal("apply without confirm must fail under the required policy")
	} else if verr, ok := err.(*validationError); !ok || verr.Code() != "BIOS_CONFIRM_REQUIRED" {
		t.Fatalf("apply gate: want BIOS_CONFIRM_REQUIRED, got %v", err)
	}
	confirm := true
	aresp, err := s.ApplyBiosProfile(ctx, gen.ApplyBiosProfileRequestObject{
		Id:   gen.ProfileId(pid),
		Body: &gen.BiosProfileApply{MachineIds: &[]string{machine}, Confirm: &confirm}})
	if err != nil {
		t.Fatalf("apply with confirm: %v", err)
	}
	ajob, ok := aresp.(gen.ApplyBiosProfile202JSONResponse)
	if !ok || ajob.Id == "" {
		t.Fatalf("apply job wrong: %+v", ajob)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE job_id = $1`, ajob.Id)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, ajob.Id)
	})

	// Delete: assignments clear via SET NULL; the read 404s.
	if _, err := s.DeleteBiosProfile(ctx, gen.DeleteBiosProfileRequestObject{Id: gen.ProfileId(pid)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.BiosProfiles.Get(ctx, pid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted profile must 404, got %v", err)
	}
}
