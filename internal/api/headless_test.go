package api

import (
	"context"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
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
