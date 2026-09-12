package api

// Machine-facing install-plan dry-run (docs/03-api.md §4 proposal): the
// business layer resolves a spec into a plan to show users BEFORE they
// confirm an install. Same validation surface as a real submission — spec
// parsing, selector resolution and the distro driver's render all run; only
// the pipeline stages are skipped and nothing is written.

import (
	"context"
	"encoding/json"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/provision"
)

// CreateInstallPlan implements POST /machines/{id}/install-plan.
func (s *Server) CreateInstallPlan(ctx context.Context, request gen.CreateInstallPlanRequestObject) (gen.CreateInstallPlanResponseObject, error) {
	machine, err := s.Machines.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	// Absent inventory is fine here: the plan reports the gap as a warning
	// and selectors fail with the same codes a real install would produce.
	var hw bmc.HardwareView
	_ = json.Unmarshal(machine.Hardware, &hw)

	raw, err := json.Marshal(*request.Body)
	if err != nil {
		return nil, err
	}
	plan, err := provision.PlanInstall(ctx, raw, &hw, s.Render)
	if err != nil {
		return nil, err
	}
	return gen.CreateInstallPlan200JSONResponse(*plan), nil
}
