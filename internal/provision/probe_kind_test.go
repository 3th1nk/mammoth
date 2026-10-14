package provision

import (
	"errors"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// An explicit probe request that the machine's BMC cannot honor must be a
// classified rejection, never a silent downgrade to the identity fallback.
func TestProbeKindReject(t *testing.T) {
	cases := []struct {
		kind     string
		proto    bmc.Protocol
		wantCode string // "" = accepted
	}{
		{"auto", bmc.ProtocolIPMI, ""},
		{"", bmc.ProtocolRedfish, ""},
		{"redfish", bmc.ProtocolRedfish, ""},
		{"redfish", bmc.ProtocolIPMI, "SCHEMA_INVALID_ACTION"},
		{"redfish", bmc.ProtocolFake, "SCHEMA_INVALID_ACTION"},
		{"bogus", bmc.ProtocolRedfish, "SCHEMA_INVALID_ACTION"},
	}
	for _, c := range cases {
		err := probeKindReject(c.kind, c.proto)
		if c.wantCode == "" {
			if err != nil {
				t.Errorf("probeKindReject(%q, %s) = %v, want nil", c.kind, c.proto, err)
			}
			continue
		}
		var ei *errInfo
		if !errors.As(err, &ei) {
			t.Errorf("probeKindReject(%q, %s) = %v, not a classified error", c.kind, c.proto, err)
			continue
		}
		if ei.Code != c.wantCode {
			t.Errorf("probeKindReject(%q, %s) code = %s, want %s", c.kind, c.proto, ei.Code, c.wantCode)
		}
	}
}
