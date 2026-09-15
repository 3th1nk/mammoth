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
