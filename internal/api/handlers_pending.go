package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// ── zero-registration surface (docs/09-roadmap.md) ─────────────────────────
// Machine face: the shared enrollment payload serves unknown MACs; the
// report endpoint records their /sys scan into pending_machines. Control
// face: pending-machines lists what the room announced by itself. Promotion
// to a registered machine (claim) is a follow-up operation.

// ReportEnrollment records an enrollment probe's /sys scan for the MAC it
// booted from. The token mismatching the configured enrollment token is
// answered 404 — indistinguishable from enrollment being off, so a probe
// aimed at the wrong deployment learns nothing.
func (s *Server) ReportEnrollment(ctx context.Context, request gen.ReportEnrollmentRequestObject) (gen.ReportEnrollmentResponseObject, error) {
	if s.Enroll == nil || s.Enroll.Token == "" || request.Token != s.Enroll.Token {
		return nil, verrStatus(http.StatusNotFound, "NETBOOT_UNKNOWN_ENROLLMENT",
			"enrollment is not available here")
	}
	mac := netboot.NormalizeMAC(request.Params.Mac)
	if mac == "" {
		return nil, verrStatus(http.StatusBadRequest, "SCHEMA_INVALID_MAC",
			"mac parameter %q is not a hardware address", request.Params.Mac)
	}
	raw, err := json.Marshal(request.Body)
	if err != nil {
		return nil, verr("SCHEMA_INVALID_PROBE_REPORT", "enrollment report is not valid JSON content")
	}
	if err := s.Pending.SaveReport(ctx, mac, raw); err != nil {
		return nil, err
	}
	obs.FromContext(ctx).InfoContext(ctx, "enrollment report recorded",
		"mac", mac, "disks", len(request.Body.Disks))
	return gen.ReportEnrollment204Response{}, nil
}

// FetchNetbootEnrollFile serves the shared enrollment boot tree — one tree
// for every unknown MAC, built once at startup. Grants work like the
// per-task boot trees (flat allowlist + subtree), confined to
// MediaDir/netboot/enroll.
func (s *Server) FetchNetbootEnrollFile(ctx context.Context, request gen.FetchNetbootEnrollFileRequestObject) (gen.FetchNetbootEnrollFileResponseObject, error) {
	if s.Enroll == nil {
		return nil, verrStatus(http.StatusNotFound, "NETBOOT_UNKNOWN_ENROLLMENT",
			"enrollment is not available here")
	}
	rel, ok := entryGrants(s.Enroll.Tree, request.Name)
	if !ok {
		return nil, verrStatus(http.StatusNotFound, "NETBOOT_UNKNOWN_ENROLLMENT",
			"file %q is not part of the enrollment tree", request.Name)
	}
	path := filepath.Join(s.MediaDir, "netboot", "enroll", rel)
	f, err := os.Open(path)
	if err != nil {
		return nil, verrStatus(http.StatusNotFound, "NETBOOT_UNKNOWN_ENROLLMENT",
			"file %q is not available yet", request.Name)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, verrStatus(http.StatusNotFound, "NETBOOT_UNKNOWN_ENROLLMENT",
			"file %q is not available", request.Name)
	}
	// Ownership transfers to the response visitor (see FetchNetbootFile —
	// deferring Close here truncated transfers on real hardware).
	return gen.FetchNetbootEnrollFile200ApplicationoctetStreamResponse{
		Body:          f,
		ContentLength: st.Size(),
	}, nil
}

// pendingToGen maps the store row to the contract shape.
func pendingToGen(m *store.PendingMachine) gen.PendingMachine {
	out := gen.PendingMachine{
		Mac:         m.MAC,
		FirstSeenAt: m.FirstSeenAt,
		LastSeenAt:  m.LastSeenAt,
	}
	if m.Firmware != nil {
		out.Firmware = m.Firmware
	}
	if len(m.Report) > 0 {
		var report map[string]any
		if json.Unmarshal(m.Report, &report) == nil {
			out.Report = &report
		}
	}
	return out
}

// ListPendingMachines returns every zero-registration sighting.
func (s *Server) ListPendingMachines(ctx context.Context, _ gen.ListPendingMachinesRequestObject) (gen.ListPendingMachinesResponseObject, error) {
	if s.Pending == nil {
		return gen.ListPendingMachines200JSONResponse{Items: []gen.PendingMachine{}}, nil
	}
	items, err := s.Pending.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gen.PendingMachine, 0, len(items))
	for _, m := range items {
		out = append(out, pendingToGen(m))
	}
	return gen.ListPendingMachines200JSONResponse{Items: out}, nil
}

// GetPendingMachine returns one sighting by MAC.
func (s *Server) GetPendingMachine(ctx context.Context, request gen.GetPendingMachineRequestObject) (gen.GetPendingMachineResponseObject, error) {
	if s.Pending == nil {
		return nil, verrStatus(http.StatusNotFound, "MACHINE_NOT_FOUND",
			"no pending sighting for %q", request.Mac)
	}
	mac := netboot.NormalizeMAC(request.Mac)
	m, err := s.Pending.Get(ctx, mac)
	if err != nil {
		return nil, err
	}
	return gen.GetPendingMachine200JSONResponse(pendingToGen(m)), nil
}

// ClaimPendingMachine promotes a sighting to a registered machine: the
// registration runs the exact POST /machines path, then the pending data
// migrates so nothing the machine reported about itself is lost — the
// firmware observation into the machine's PXE observation columns, the
// /sys scan into its first layout snapshot (source=ramdisk, docs/
// 05-inventory.md §5) — and finally the pending row is consumed.
//
// The migration steps are best-effort by design: the claim's primary
// effect is the machine; a failed migration is logged, never fatal, and
// the auto-discovery job refreshes the same facts anyway. The final delete
// is last so a mid-way failure leaves the sighting inspectable.
func (s *Server) ClaimPendingMachine(ctx context.Context, request gen.ClaimPendingMachineRequestObject) (gen.ClaimPendingMachineResponseObject, error) {
	if s.Pending == nil {
		return nil, verrStatus(http.StatusNotFound, "MACHINE_NOT_FOUND",
			"no pending sighting for %q", request.Mac)
	}
	mac := netboot.NormalizeMAC(request.Mac)
	pending, err := s.Pending.Get(ctx, mac)
	if err != nil {
		return nil, err
	}
	m, err := s.registerMachine(ctx, request.Body)
	if err != nil {
		// A bmc_address conflict surfaces here (409) — the sighting stays
		// pending and inspectable.
		return nil, err
	}
	if pending.Firmware != nil || !pending.LastSeenAt.IsZero() {
		if err := s.Machines.SetPXEObservation(ctx, m.ID, pending.Firmware, &pending.LastSeenAt); err != nil {
			obs.FromContext(ctx).WarnContext(ctx, "claim: pxe observation migration failed",
				obs.FieldMachineID, m.ID, "err", err.Error())
		}
	}
	if len(pending.Report) > 0 {
		if _, err := s.Machines.SaveLayout(ctx, m.ID, "ramdisk", pending.Report, 0); err != nil {
			obs.FromContext(ctx).WarnContext(ctx, "claim: probe report migration failed",
				obs.FieldMachineID, m.ID, "err", err.Error())
		}
	}
	if err := s.Pending.Delete(ctx, mac); err != nil {
		obs.FromContext(ctx).WarnContext(ctx, "claim: pending row removal failed",
			"mac", mac, "err", err.Error())
	}
	s.Events.Append(ctx, "machine", m.ID, "machine.claimed", map[string]any{
		"mac": mac, "firmware": pending.Firmware,
	})
	obs.FromContext(ctx).InfoContext(ctx, "pending sighting claimed",
		obs.FieldMachineID, m.ID, "mac", mac)
	return gen.ClaimPendingMachine201JSONResponse(machineOut(m)), nil
}
