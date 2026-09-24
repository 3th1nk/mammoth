package api

import (
	"context"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
)

// The batch-labels request gates run before the store sees anything: at
// least one machine, at least one of add/remove non-empty, and no key named
// in both (the SQL applies remove first, so a clash would silently let add
// win — rejected instead, docs/04 §A5).
func TestBatchUpdateMachineLabelsValidation(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	ids := []gen.MachineId{"mch_x"}
	cases := []struct {
		name string
		body *gen.BatchLabelUpdate
		want string
	}{
		{"nil body", nil, "SCHEMA_INVALID_BATCH_LABELS"},
		{"no machines", &gen.BatchLabelUpdate{MachineIds: []gen.MachineId{}}, "SCHEMA_INVALID_BATCH_LABELS"},
		{"both empty", &gen.BatchLabelUpdate{MachineIds: ids}, "SCHEMA_INVALID_BATCH_LABELS"},
		{"key clash", &gen.BatchLabelUpdate{
			MachineIds: ids,
			Add:        ptrMap(map[string]string{"env": "prod"}),
			Remove:     ptrSlice([]string{"env"}),
		}, "SCHEMA_INVALID_BATCH_LABELS"},
	}
	for _, tc := range cases {
		_, err := s.BatchUpdateMachineLabels(ctx, gen.BatchUpdateMachineLabelsRequestObject{Body: tc.body})
		ve, ok := err.(*validationError)
		if !ok || ve.Code() != tc.want {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.want)
		}
	}
}

func ptrMap(m map[string]string) *map[string]string { return &m }
func ptrSlice(s []string) *[]string                 { return &s }
