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
