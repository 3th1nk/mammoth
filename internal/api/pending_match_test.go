package api

// The MAC-match-first rule at every pending ingestion path (docs/
// 09-roadmap.md zero-registration ledger): a sighting whose MAC the
// hardware view owns belongs to a registered machine — it updates that
// machine's PXE observation columns and never enters pending_machines,
// however it arrives (builtin OnObserve, the edge observation feed, the
// enrollment probe's report, the iPXE script fallback). The manual dismiss
// endpoint covers rows written before their machine was registered.
// PG-gated via MAMMOTH_TEST_PG_DSN (`make test-pg`).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

func TestPendingIngestionMatchFirst(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping pending match-first suite")
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
	const ownedMAC = "aa:bb:cc:dd:ee:91"  // NIC of the seeded machine
	const strayMAC = "aa:bb:cc:dd:ee:92"  // never registered
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM pending_machines WHERE mac IN ($1, $2)`, ownedMAC, strayMAC)
		_, _ = db.Exec(`DELETE FROM events WHERE resource_id IN ($1, $2)`, ownedMAC, strayMAC)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, "tst_match_m_")
	})
	hw := `{"nics":[{"mac":"aa:bb:cc:dd:ee:91","name":"eno1"}]}`
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, state, hardware)
		VALUES ('tst_match_m_', '{}'::jsonb, 'fake://match', 'none', 'ready', $1::jsonb)`, hw); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	s := &Server{Deps{
		Machines: store.NewMachineRepo(db),
		Events:   store.NewEventRepo(db),
		Pending:  store.NewPendingRepo(db),
		Enroll:   &Enrollment{Token: "tok"},
	}}

	pendingRowCount := func(t *testing.T, mac string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pending_machines WHERE mac = $1`, mac).Scan(&n); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		return n
	}

	// Registered MAC, probe report: answered so the probe completes, ledger
	// stays clean.
	resp, err := s.ReportEnrollment(ctx, gen.ReportEnrollmentRequestObject{
		Token:  "tok",
		Params: gen.ReportEnrollmentParams{Mac: ownedMAC},
		Body: &gen.ProbeReport{Disks: []map[string]any{
			{"device": "nvme0n1", "size_bytes": 123},
		}},
	})
	if err != nil {
		t.Fatalf("registered-mac report: %v", err)
	}
	if _, ok := resp.(gen.ReportEnrollment204Response); !ok {
		t.Fatalf("registered-mac report response wrong: %T", resp)
	}
	if n := pendingRowCount(t, ownedMAC); n != 0 {
		t.Fatalf("registered MAC must not enter pending_machines, got %d rows", n)
	}

	// Unknown MAC, probe report: the ledger takes it as before.
	if _, err := s.ReportEnrollment(ctx, gen.ReportEnrollmentRequestObject{
		Token:  "tok",
		Params: gen.ReportEnrollmentParams{Mac: strayMAC},
		Body: &gen.ProbeReport{Disks: []map[string]any{
			{"device": "nvme0n1", "size_bytes": 456},
		}},
	}); err != nil {
		t.Fatalf("stray-mac report: %v", err)
	}
	if n := pendingRowCount(t, strayMAC); n != 1 {
		t.Fatalf("stray MAC report must ledger, got %d rows", n)
	}

	// Registered MAC via the edge observation feed: machine observation only.
	// The alternate MAC spelling (dash form, upper case) proves the match is
	// the same normalized jsonb test the builtin hook runs.
	gin.SetMode(gin.TestMode)
	observe := func(mac string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/netboot/observe",
			strings.NewReader(`{"mac":"`+mac+`","firmware":"uefi-x64"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		s.ObservePXEClient(c)
		// The handler method runs without the engine's flush: the status
		// lives on the writer, not the recorder.
		return c.Writer.Status()
	}
	if code := observe("AA-BB-CC-DD-EE-91"); code != http.StatusNoContent {
		t.Fatalf("edge observe registered mac: got %d", code)
	}
	if n := pendingRowCount(t, ownedMAC); n != 0 {
		t.Fatalf("registered MAC edge sighting must not enter pending_machines, got %d rows", n)
	}
	var pxeFirmware *string
	if err := db.QueryRowContext(ctx,
		`SELECT pxe_firmware FROM machines WHERE id = 'tst_match_m_'`).Scan(&pxeFirmware); err != nil {
		t.Fatalf("read pxe observation: %v", err)
	}
	if pxeFirmware == nil || *pxeFirmware != "uefi-x64" {
		t.Fatalf("edge sighting must update the machine's pxe observation, got %v", pxeFirmware)
	}

	// Unknown MAC via the edge feed: ledger row created.
	if code := observe(strayMAC); code != http.StatusNoContent {
		t.Fatalf("edge observe stray mac: got %d", code)
	}
	if n := pendingRowCount(t, strayMAC); n != 1 {
		t.Fatalf("stray MAC edge sighting must ledger, got %d rows", n)
	}

	// Manual dismiss: one-time removal, idempotent.
	del, err := s.DeletePendingMachine(ctx, gen.DeletePendingMachineRequestObject{Mac: strayMAC})
	if err != nil {
		t.Fatalf("dismiss stray sighting: %v", err)
	}
	if _, ok := del.(gen.DeletePendingMachine204Response); !ok {
		t.Fatalf("dismiss response wrong: %T", del)
	}
	if n := pendingRowCount(t, strayMAC); n != 0 {
		t.Fatalf("dismiss must remove the row, got %d", n)
	}
	if _, err := s.DeletePendingMachine(ctx, gen.DeletePendingMachineRequestObject{Mac: strayMAC}); err != nil {
		t.Fatalf("dismiss is idempotent: %v", err)
	}
}
