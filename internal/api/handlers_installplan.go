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
	// Health advisory (the report档 surface of policy.health_gate, docs
	// 04-install-spec.md §5.6): the plan is where an operator looks BEFORE
	// submitting, so an explicitly failed disk reading in the machine's
	// latest layout snapshot always surfaces here as a warning — the plan
	// stays advisory; blocking is the submission's job.
	if raw, _, lerr := s.Machines.LatestLayout(ctx, machine.ID); lerr == nil {
		var snap struct {
			Disks []struct {
				Device string `json:"device"`
				Serial string `json:"serial"`
				Health string `json:"health"`
			} `json:"disks"`
		}
		if json.Unmarshal(raw, &snap) == nil {
			for _, d := range snap.Disks {
				if d.Health != "fail" {
					continue
				}
				id := d.Serial
				if id == "" {
					id = d.Device
				}
				w := "disk " + id + " reported health=fail in the latest probe snapshot — consider policy.health_gate=block or replacing the disk"
				if plan.Warnings == nil {
					plan.Warnings = &[]string{w}
				} else {
					*plan.Warnings = append(*plan.Warnings, w)
				}
			}
		}
	}
	return gen.CreateInstallPlan200JSONResponse(*plan), nil
}
