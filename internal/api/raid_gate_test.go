package api

import (
	"testing"
)

// The RAID volume lifecycle actions' API gate (docs/07-bmc.md §6.7): both
// destroy data, so the explicit confirm flag is always required (no
// deployment opt-out); create's level/member-count rules mirror the
// install-side checkRaidShape so a bad shape rejects at submit rather than
// dying at the BMC; delete keys on the controller-assigned name.
func TestValidateRaidConfirm(t *testing.T) {
	s := &Server{}

	cases := []struct {
		name  string
		raw   string
		aType string
		want  string // "" = accepted
	}{
		{"create without confirm", `{"type":"create_raid_volume","level":"1","serials":["A","B"]}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"create ok", `{"type":"create_raid_volume","level":"1","serials":["A","B"],"confirm":true}`, "create_raid_volume", ""},
		{"create level 0 single member", `{"type":"create_raid_volume","level":"0","serials":["A"],"confirm":true}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"create level 1 five members", `{"type":"create_raid_volume","level":"1","serials":["A","B","C","D","E"],"confirm":true}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"create level 5 two members", `{"type":"create_raid_volume","level":"5","serials":["A","B"],"confirm":true}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"create level 10 odd count", `{"type":"create_raid_volume","level":"10","serials":["A","B","C","D","E"],"confirm":true}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"create level 10 four members ok", `{"type":"create_raid_volume","level":"10","serials":["A","B","C","D"],"confirm":true}`, "create_raid_volume", ""},
		{"create unknown level", `{"type":"create_raid_volume","level":"6","serials":["A","B"],"confirm":true}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"create duplicate serials", `{"type":"create_raid_volume","level":"1","serials":["A","A"],"confirm":true}`, "create_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"delete without confirm", `{"type":"delete_raid_volume","name":"LogicalDrive0"}`, "delete_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"delete without name", `{"type":"delete_raid_volume","confirm":true}`, "delete_raid_volume", "SCHEMA_INVALID_ACTION"},
		{"delete ok", `{"type":"delete_raid_volume","name":"LogicalDrive0","confirm":true}`, "delete_raid_volume", ""},
	}
	for _, tc := range cases {
		err := s.validateRaidConfirm([]byte(tc.raw), tc.aType)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected err %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: want %s, got nil", tc.name, tc.want)
			continue
		}
		ve, ok := err.(*validationError)
		if !ok || ve.Code() != tc.want {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.want)
		}
	}
}
