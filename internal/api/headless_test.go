package api

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/store"
)

// The headless (protocol=none) registration gates (docs/13-headless-design.md
// §2): the address must carry the identity MAC in the none:// form, no
// credential may ride along, and the address normalizes onto the canonical
// colon spelling so one machine is one row under UNIQUE(bmc_address).
func TestRegisterHeadlessValidation(t *testing.T) {
	s := &Server{}
	proto := gen.BMCProtocolNone
	ctx := context.Background()

	cases := []struct {
		name    string
		address string
		want    string
	}{
		{"bare mac without scheme", "aa:bb:cc:dd:ee:01", "SCHEMA_INVALID_MACHINE"},
		{"scheme with garbage mac", "none://hello", "SCHEMA_INVALID_MACHINE"},
		{"empty mac", "none://", "SCHEMA_INVALID_MACHINE"},
	}
	for _, tc := range cases {
		_, err := s.registerMachine(ctx, &gen.MachineCreate{
			Bmc: gen.BMC{Address: tc.address, Protocol: &proto},
		})
		ve, ok := err.(*validationError)
		if !ok || ve.Code() != tc.want {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.want)
		}
	}

	// The canonicalization: whatever spelling came in, the stored address is
	// none://<normalized-mac>. (Fails before the store touch on a nil Server
	// only after validation — so assert via the pure helper.)
	if got := netboot.HeadlessMAC("none://AA-BB-CC-DD-EE-01"); got != "aa:bb:cc:dd:ee:01" {
		t.Errorf("HeadlessMAC normalization = %q", got)
	}
	if got := netboot.HeadlessMAC("none://aabbccddeeff"); got != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("HeadlessMAC bare form = %q", got)
	}
	if got := netboot.HeadlessMAC("https://bmc.example.com"); got != "" {
		t.Errorf("non-headless address must not parse, got %q", got)
	}
}

// The claim's hardware projection (docs/13-headless-design.md §3.1): disks
// and NICs of the probe report become the machine's hardware view with
// coverage=partial — the view PXE observation and netboot arming key on for
// headless machines. Old reports (no nics) still project their disks;
// garbage yields nil so the claim just skips the projection.
func TestHardwareFromReport(t *testing.T) {
	hw := hardwareFromReport([]byte(`{
		"captured_at":"2026-11-04T00:00:00Z","source":"ramdisk",
		"disks":[{"device":"sda","size_bytes":250059350016,"serial":"S3Z4","partitions":[]}],
		"nics":[{"name":"eno1","mac":"aa:bb:cc:dd:ee:01","link_up":true},
		        {"name":"eno2","mac":"AA-BB-CC-DD-EE-02","link_up":false},
		        {"name":"bad","mac":"nonsense"}]
	}`))
	if hw == nil || hw.Coverage != "partial" {
		t.Fatalf("hardware = %+v, want partial coverage", hw)
	}
	if len(hw.Disks) != 1 || hw.Disks[0].Name != "sda" || hw.Disks[0].SizeBytes != 250059350016 {
		t.Errorf("disks = %+v", hw.Disks)
	}
	if len(hw.NICs) != 2 || hw.NICs[0].MAC != "aa:bb:cc:dd:ee:01" || !hw.NICs[0].LinkUp {
		t.Errorf("nics = %+v", hw.NICs)
	}
	if hw.NICs[1].MAC != "aa:bb:cc:dd:ee:02" || hw.NICs[1].LinkUp {
		t.Errorf("nic normalization/link_up = %+v", hw.NICs[1])
	}

	if hw := hardwareFromReport([]byte(`{"disks":[{"device":"sda","size_bytes":1}]}`)); hw == nil || len(hw.NICs) != 0 {
		t.Errorf("disk-only report must project, got %+v", hw)
	}
	if hw := hardwareFromReport([]byte(`not json`)); hw != nil {
		t.Errorf("garbage report must yield nil, got %+v", hw)
	}
	if hw := hardwareFromReport([]byte(`{"disks":[],"nics":[]}`)); hw != nil {
		t.Errorf("empty scan must yield nil, got %+v", hw)
	}
}

// The protocol/address/credential triple must stay consistent across PATCHes
// that cross the headless boundary in either direction.
func TestValidateBMCShape(t *testing.T) {
	cases := []struct {
		name       string
		protocol   string
		address    string
		credential string
		want       string // "" = ok
	}{
		{"headless ok", "none", "none://aa:bb:cc:dd:ee:01", "", ""},
		{"headless with credential", "none", "none://aa:bb:cc:dd:ee:01", "cred_x", "SCHEMA_INVALID_MACHINE"},
		{"headless wrong address", "none", "192.168.1.10", "", "SCHEMA_INVALID_MACHINE"},
		{"controller ok", "redfish", "https://bmc", "cred_x", ""},
		{"controller without credential", "redfish", "https://bmc", "", "SCHEMA_INVALID_MACHINE"},
		{"controller with none address", "ipmi", "none://aa:bb:cc:dd:ee:01", "cred_x", "SCHEMA_INVALID_MACHINE"},
	}
	for _, tc := range cases {
		err := validateBMCShape(tc.protocol, tc.address, tc.credential)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected err %v", tc.name, err)
			}
			continue
		}
		ve, ok := err.(*validationError)
		if !ok || ve.Code() != tc.want {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.want)
		}
	}
}

// The headless submission gate (docs/13-headless-design.md §2/§4): PXE is
// the only carrier, hardware RAID needs the BMC they don't have. Both
// pre-fail the machine's task; non-headless machines sail through.
func TestValidateHeadlessShape(t *testing.T) {
	s := &Server{Deps: Deps{BootStrategyDefault: "virtual_media"}}
	ctx := context.Background()
	noneMch, redMch := "mch_none", "mch_red"

	// Machines behind the gate.
	machines := map[string]*store.Machine{
		noneMch: {ID: noneMch, BMCProtocol: "none"},
		redMch:  {ID: redMch, BMCProtocol: "redfish"},
	}
	s.Machines = fakeMachineGetter(t, machines)

	spec := func(boot string, raidMode string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{
			"boot": map[string]any{"strategy": boot},
			"storage": map[string]any{
				"raid": []map[string]any{{"name": "vd0", "level": "1", "mode": raidMode}},
			},
		})
		return raw
	}

	codeOf := func(err error) string {
		ve, ok := err.(*validationError)
		if !ok {
			return err.Error()
		}
		return ve.Code()
	}

	if got := codeOf(s.validateHeadlessShape(ctx, noneMch, spec("", ""))); got != "SCHEMA_UNSUPPORTED_BOOT_STRATEGY" {
		t.Errorf("deployment default virtual_media must reject for none machines, got %q", got)
	}
	if err := s.validateHeadlessShape(ctx, noneMch, spec("virtual_media", "")); err == nil {
		t.Error("explicit virtual_media must reject for none machines")
	}
	if err := s.validateHeadlessShape(ctx, noneMch, spec("pxe", "")); err != nil {
		t.Errorf("pxe must pass: %v", err)
	}
	if got := codeOf(s.validateHeadlessShape(ctx, noneMch, spec("pxe", "hardware"))); got != "SCHEMA_INVALID_STORAGE" {
		t.Errorf("hardware raid must reject, got %q", got)
	}
	if err := s.validateHeadlessShape(ctx, noneMch, spec("pxe", "software")); err != nil {
		t.Errorf("software raid must pass: %v", err)
	}
	// Non-headless machines are not this gate's subject — virtual_media +
	// hardware raid pass untouched (their own gates live elsewhere).
	if err := s.validateHeadlessShape(ctx, redMch, spec("virtual_media", "hardware")); err != nil {
		t.Errorf("non-headless machine must pass untouched: %v", err)
	}
}

// fakeMachineGetter builds a minimal MachineRepo-backed Server.Machines for
// gates that only read machine rows — backed by a real PG when available,
// skipped otherwise (the gates are read-only consumers).
func fakeMachineGetter(t *testing.T, machines map[string]*store.Machine) *store.MachineRepo {
	t.Helper()
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping gate suite needing machine rows")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := store.NewMachineRepo(db)
	// The gate suite's controller machines need a real credential row
	// (credentials FK); its secret is undecryptable — the gate never
	// decrypts, it only reads the protocol column.
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ('cred_gate_test', 'gate-suite', 'bmc', '\x00'::bytea)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = 'cred_gate_test'`)
	})
	for id, m := range machines {
		addr := "fake://" + id
		credID := any("cred_gate_test")
		if m.BMCProtocol == "none" {
			addr = "none://52:54:00:ga:te:" + id[len(id)-2:]
			credID = nil // headless machines carry no BMC credential
		}
		if _, err := db.Exec(`
			INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
			VALUES ($1, '{}'::jsonb, $2, $3, $4, 'ready')`,
			id, addr, m.BMCProtocol, credID); err != nil {
			t.Fatalf("seed machine %s: %v", id, err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, id)
		})
	}
	return repo
}
