package api

// BIOS baseline profiles (docs/07-bmc.md §6.2, docs/09-roadmap.md 下一阶段
// 13): named declarative attribute sets above the BiosSetter single-machine
// write — the drift inspection's reference, the batch apply's payload
// source. Pure capability composition: no new BMC face; the two-stage
// confirmation contract applies to the apply fanout unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/store"
)

func profileToGen(p *store.BiosProfile) gen.BiosProfile {
	return gen.BiosProfile{
		Id:         p.ID,
		Name:       p.Name,
		Attributes: p.Attributes,
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
	}
}

// CreateBiosProfile persists a named baseline. Name uniqueness is the
// UNIQUE constraint's business (409); an empty attribute set is rejected —
// a baseline that declares nothing is a typo wearing a name.
func (s *Server) CreateBiosProfile(ctx context.Context, request gen.CreateBiosProfileRequestObject) (gen.CreateBiosProfileResponseObject, error) {
	body := request.Body
	if body == nil || body.Name == "" {
		return nil, verr("SCHEMA_INVALID_PROFILE", "name is required")
	}
	if len(body.Attributes) == 0 {
		return nil, verr("SCHEMA_INVALID_PROFILE", "attributes must be a non-empty object — an empty baseline is a typo wearing a name")
	}
	p := &store.BiosProfile{Name: body.Name, Attributes: body.Attributes}
	if err := s.BiosProfiles.Create(ctx, p); err != nil {
		if isProfileNameTaken(err) {
			return nil, verrStatus(http.StatusConflict, "BIOS_PROFILE_NAME_TAKEN",
				"a profile named %q already exists", body.Name)
		}
		return nil, err
	}
	s.Events.Append(ctx, "bios_profile", p.ID, "bios_profile_created", map[string]any{
		"name": p.Name, "attributes": len(p.Attributes),
	})
	return gen.CreateBiosProfile201JSONResponse(profileToGen(p)), nil
}

func (s *Server) ListBiosProfiles(ctx context.Context, request gen.ListBiosProfilesRequestObject) (gen.ListBiosProfilesResponseObject, error) {
	profiles, err := s.BiosProfiles.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]gen.BiosProfile, 0, len(profiles))
	for _, p := range profiles {
		items = append(items, profileToGen(p))
	}
	return gen.ListBiosProfiles200JSONResponse{Items: &items}, nil
}

func (s *Server) GetBiosProfile(ctx context.Context, request gen.GetBiosProfileRequestObject) (gen.GetBiosProfileResponseObject, error) {
	p, err := s.BiosProfiles.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetBiosProfile200JSONResponse(profileToGen(p)), nil
}

func (s *Server) DeleteBiosProfile(ctx context.Context, request gen.DeleteBiosProfileRequestObject) (gen.DeleteBiosProfileResponseObject, error) {
	if err := s.BiosProfiles.Delete(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "bios_profile", string(request.Id), "bios_profile_deleted", nil)
	return gen.DeleteBiosProfile204Response{}, nil
}

// SetMachineBiosBaseline assigns (or clears) the machine's baseline. The
// profile must exist (a typo'd id would silently dead-end every later
// drift read); clearing needs no existence dance.
func (s *Server) SetMachineBiosBaseline(ctx context.Context, request gen.SetMachineBiosBaselineRequestObject) (gen.SetMachineBiosBaselineResponseObject, error) {
	machine, err := s.Machines.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	body := request.Body
	if body == nil || body.ProfileId == nil {
		return nil, verr("SCHEMA_INVALID_BASELINE", "profile_id is required (null clears — send it explicitly)")
	}
	if pid := *body.ProfileId; pid != "" {
		if _, err := s.BiosProfiles.Get(ctx, pid); err != nil {
			return nil, verr("SCHEMA_UNKNOWN_PROFILE", "unknown profile %q", pid)
		}
		machine.BIOSProfileID = &pid
		if err := s.Machines.SetBiosProfile(ctx, machine.ID, &pid); err != nil {
			return nil, err
		}
	} else {
		machine.BIOSProfileID = nil
		if err := s.Machines.SetBiosProfile(ctx, machine.ID, nil); err != nil {
			return nil, err
		}
	}
	fresh, err := s.Machines.Get(ctx, machine.ID)
	if err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "machine", machine.ID, "machine.bios_baseline_set", map[string]any{
		"profile_id": machine.BIOSProfileID,
	})
	return gen.SetMachineBiosBaseline200JSONResponse(machineOut(fresh)), nil
}

// GetMachineBiosDrift live-reads the machine's BIOS attributes (the same
// BiosSetter path as /bios) and holds them against the assigned baseline —
// the runner's own write semantics on the read side
// (provision.CompareBiosDrift). No baseline → {"drift": null}.
func (s *Server) GetMachineBiosDrift(ctx context.Context, request gen.GetMachineBiosDriftRequestObject) (gen.GetMachineBiosDriftResponseObject, error) {
	machine, err := s.Machines.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	if machine.BIOSProfileID == nil {
		return gen.GetMachineBiosDrift200JSONResponse(gen.BiosDriftView{}), nil
	}
	profile, err := s.BiosProfiles.Get(ctx, *machine.BIOSProfileID)
	if err != nil {
		return nil, err
	}
	cred, addr, proto, err := s.outOfBandFor(ctx, machine.ID)
	if err != nil {
		return nil, err
	}
	res, err := s.BMC.Do(ctx, addr, cred, proto, "bios_attributes", func(ctx context.Context, d bmc.Driver) (any, error) {
		bs, ok := d.(bmc.BiosSetter)
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "bios_attributes"}
		}
		return bs.BiosAttributes(ctx, addr, cred)
	})
	if err != nil {
		return nil, err
	}
	attrs, _ := res.(map[string]any)
	drift := provision.CompareBiosDrift(attrs, profile.Attributes)
	out := gen.BiosDriftView{}
	if drift != nil {
		verdict := gen.BiosDrift{
			Matched: drift.Matched,
			Unknown: &drift.Unknown,
		}
		if drift.Drifted != nil {
			d := map[string][]any{}
			for k, pair := range drift.Drifted {
				d[k] = pair
			}
			verdict.Drifted = &d
		}
		out.Drift = &verdict
	}
	s.Events.Append(ctx, "machine", machine.ID, "machine.bios_drift_checked", map[string]any{
		"profile_id": profile.ID, "matched": drift.Matched,
		"drifted": len(drift.Drifted), "unknown": len(drift.Unknown),
	})
	return gen.GetMachineBiosDrift200JSONResponse(out), nil
}

// ApplyBiosProfile fans the profile's attributes out as ONE batch power
// job — one set_bios_attributes task per target machine. The two-stage
// contract applies unchanged (confirm gate, runner live-table re-read);
// explicit targets or the profile's assigned machines, never both empty.
func (s *Server) ApplyBiosProfile(ctx context.Context, request gen.ApplyBiosProfileRequestObject) (gen.ApplyBiosProfileResponseObject, error) {
	profile, err := s.BiosProfiles.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	body := request.Body
	if body == nil {
		return nil, verr("SCHEMA_INVALID_APPLY", "request body required")
	}
	var machineIDs []string
	if body.MachineIds != nil && len(*body.MachineIds) > 0 {
		machineIDs = *body.MachineIds
	} else {
		machineIDs, err = s.BiosProfiles.AssignedMachines(ctx, profile.ID)
		if err != nil {
			return nil, err
		}
	}
	if len(machineIDs) == 0 {
		return nil, verr("SCHEMA_INVALID_APPLY",
			"no target machines — pass machine_ids or assign the profile (PUT /machines/{id}/bios/baseline)")
	}
	if s.BiosConfirmRequired && (body.Confirm == nil || !*body.Confirm) {
		return nil, verrStatus(http.StatusUnprocessableEntity, "BIOS_CONFIRM_REQUIRED",
			"applying a BIOS baseline is high-risk and requires \"confirm\": true (deployment policy %q)", "required")
	}
	if err := s.verifyMachinesExist(ctx, machineIDs); err != nil {
		return nil, err
	}
	action := map[string]any{
		"type":       "set_bios_attributes",
		"attributes": profile.Attributes,
		"confirm":    true, // the gate ran above; the runner's own check sees an explicit payload
	}
	raw, _ := json.Marshal(action)
	job, err := s.createJobRecord(ctx, createJobRecord{
		jobType:    "power",
		flow:       provision.FlowPower,
		machineIDs: machineIDs,
		actionRaw:  raw,
		requestRaw: mustJSON(body),
		createdBy:  s.subject(ctx),
	})
	if err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "bios_profile", profile.ID, "bios_profile_applied", map[string]any{
		"job_id": job.Id, "machines": len(machineIDs),
	})
	return gen.ApplyBiosProfile202JSONResponse(*job), nil
}

// isProfileNameTaken reports the bios_profiles name UNIQUE violation
// (store credentials.go has the same pgconn check, unexported there).
func isProfileNameTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
