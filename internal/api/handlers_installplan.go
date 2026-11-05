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
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
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

	// Same resolution pipeline as a real submission (docs/04-install-spec.md
	// §5.3): bindings substitute into the template, post-substitution
	// validation runs — a plan that resolves here installs there.
	ctxv := varContext{
		index:      1,
		machineID:  machine.ID,
		bmcAddress: machine.BMCAddress,
		labels:     machine.Labels,
		vars:       derefOr(request.Body.Vars, nil),
	}
	if machine.SerialNumber != nil {
		ctxv.serial = *machine.SerialNumber
	}
	specRaw, err := json.Marshal(request.Body.Spec)
	if err != nil {
		return nil, verr("SCHEMA_INVALID_SPEC", "spec is not valid: %v", err)
	}
	resolved, err := resolveSpecVars(specRaw, ctxv)
	if err != nil {
		return nil, verr("SCHEMA_UNRESOLVED_VAR",
			"machine %s: %v — every {var:…} in the spec needs a binding in vars", machine.ID, err)
	}
	if err := validateResolvedNetwork(resolved); err != nil {
		return nil, verr("SCHEMA_INVALID_SPEC", "machine %s: %v", machine.ID, err)
	}
	// Library refs resolve exactly like a real submission (resolveImageRefs
	// is the submit-side gate): image.id → file path + sha256 checksum. The
	// plan must reject a fetching/failed image the same way the submission
	// would — same validation surface, docs/03-api.md §4.
	resolved, err = s.resolveImageRefs(ctx, resolved)
	if err != nil {
		return nil, err
	}
	// Layout snapshot rides along: its LVM facts back the keep:vg bindings
	// (the executor reads the same snapshot in verify_layout), and its disk
	// health readings surface as advisory warnings below.
	var snapLvm []inbandssh.VgFacts
	raw, _, lerr := s.Machines.LatestLayout(ctx, machine.ID)
	if lerr == nil {
		var snapMeta struct {
			Lvm []inbandssh.VgFacts `json:"lvm,omitempty"`
		}
		if json.Unmarshal(raw, &snapMeta) == nil {
			snapLvm = snapMeta.Lvm
		}
	}
	plan, err := provision.PlanInstall(ctx, resolved, &hw, snapLvm, s.Render)
	if err != nil {
		return nil, err
	}
	// Health advisory (the report档 surface of policy.health_gate, docs
	// 04-install-spec.md §5.6): the plan is where an operator looks BEFORE
	// submitting, so an explicitly failed disk reading in the machine's
	// latest layout snapshot always surfaces here as a warning — the plan
	// stays advisory; blocking is the submission's job.
	if lerr == nil {
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
