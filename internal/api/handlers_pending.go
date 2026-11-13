package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
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
	// A MAC the hardware view owns belongs to a registered machine (the
	// script fallback above makes this exotic — an in-flight enroll served
	// before the machine was registered). Its facts live in the machine's
	// own hardware/layout views; answer the probe so it completes, but keep
	// the zero-registration ledger clean. Match errors fall through to the
	// old behavior (availability first).
	owned, oerr := s.Machines.HasMAC(ctx, mac)
	if oerr != nil {
		obs.FromContext(ctx).WarnContext(ctx, "enrollment report mac check failed",
			"mac", mac, "err", oerr.Error())
	}
	if owned {
		obs.FromContext(ctx).InfoContext(ctx,
			"enrollment report ignored — MAC belongs to a registered machine", "mac", mac)
		return gen.ReportEnrollment204Response{}, nil
	}
	if err := s.Pending.SaveReport(ctx, mac, raw); err != nil {
		return nil, err
	}
	// Once per probe boot — not a flood path like the PXE DISCOVER stream.
	// The console's live pending feed keys on these two event types.
	s.Events.Append(ctx, "pending", mac, "pending.reported", map[string]any{
		"disks": len(request.Body.Disks),
	})
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
	// Same Range answer as FetchNetbootFile — the enroll tree carries the
	// full kernel+initrd for every unknown MAC in the room, same resume
	// economics (see rangeFetch for the nil-response mechanics).
	if gc := rangeFetch(ctx); gc != nil {
		defer f.Close()
		serveRangeAware(gc, f, st)
		return nil, nil
	}
	// Ownership transfers to the response visitor (see FetchNetbootFile —
	// deferring Close here truncated transfers on real hardware).
	return gen.FetchNetbootEnrollFile200ApplicationoctetStreamResponse{
		Body:          f,
		ContentLength: st.Size(),
	}, nil
}

// hardwareFromReport projects an enrollment probe's /sys scan onto the
// machine's hardware view. Tolerant by design: a report without nics (old
// probes) still projects its disks; malformed JSON or an empty scan yields
// nil and the claim simply skips the projection (the layout snapshot keeps
// the verbatim report either way).
func hardwareFromReport(report json.RawMessage) *bmc.HardwareView {
	var probe struct {
		Disks []struct {
			Device    string `json:"device"`
			SizeBytes int64  `json:"size_bytes"`
			Serial    string `json:"serial"`
			Model     string `json:"model"`
		} `json:"disks"`
		NICs []struct {
			Name   string `json:"name"`
			MAC    string `json:"mac"`
			LinkUp *bool  `json:"link_up"`
		} `json:"nics"`
	}
	if json.Unmarshal(report, &probe) != nil {
		return nil
	}
	if len(probe.Disks) == 0 && len(probe.NICs) == 0 {
		return nil
	}
	hw := &bmc.HardwareView{Coverage: bmc.CoveragePartial,
		CoverageNotes: []string{"facts from the enrollment probe's /sys scan; controller-only fields absent (headless machine)"}}
	for _, d := range probe.Disks {
		if d.Device == "" {
			continue
		}
		hw.Disks = append(hw.Disks, bmc.DiskView{
			Name: d.Device, Serial: d.Serial, SizeBytes: d.SizeBytes,
		})
	}
	for _, n := range probe.NICs {
		mac := netboot.NormalizeMAC(n.MAC)
		if len(mac) != 17 {
			continue
		}
		nic := bmc.NICView{Name: n.Name, MAC: mac}
		if n.LinkUp != nil {
			nic.LinkUp = *n.LinkUp
		}
		hw.NICs = append(hw.NICs, nic)
	}
	return hw
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

// DeletePendingMachine removes one sighting — the manual answer for a row
// whose machine was registered by hand after the sighting landed. Every
// ingestion path auto-matches registered machines' NIC MACs, so without
// this such leftovers would sit in the list forever. Idempotent: removing
// an absent sighting is a no-op; an unregistered machine re-creates the
// sighting on its next PXE boot (the ledger records the room's present).
func (s *Server) DeletePendingMachine(ctx context.Context, request gen.DeletePendingMachineRequestObject) (gen.DeletePendingMachineResponseObject, error) {
	if s.Pending == nil {
		return gen.DeletePendingMachine204Response{}, nil
	}
	mac := netboot.NormalizeMAC(request.Mac)
	if err := s.Pending.Delete(ctx, mac); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "pending", mac, "pending.dismissed", map[string]any{})
	obs.FromContext(ctx).InfoContext(ctx, "pending sighting dismissed", "mac", mac)
	return gen.DeletePendingMachine204Response{}, nil
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
	// Headless claim (docs/13-headless-design.md §3.1): the claim body
	// must explicitly carry bmc.protocol=none — the sighting tells you
	// WHICH machine this is, and promoting it to a BMC-less registration
	// is an operator decision, never an accident of an omitted field.
	// The identity MAC is the pending row's (the machine booted PXE with
	// it): the body address is rewritten onto none://<mac> so the claim
	// lands on the same machine the room announced, however the request
	// spelled the address. Controller claims (redfish/ipmi/auto) keep the
	// plain POST /machines path untouched.
	if request.Body != nil &&
		request.Body.Bmc.Protocol != nil && *request.Body.Bmc.Protocol == gen.BMCProtocolNone {
		request.Body.Bmc.Address = netboot.HeadlessMACPrefix + mac
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
	reported := false
	if len(pending.Report) > 0 {
		// Project the probe's own facts onto the machine's hardware view
		// (docs/13-headless-design.md §3.1): disks and NICs with
		// coverage=partial — the controller-only fields stay empty, no
		// conclusions invented. For a headless machine this view is
		// load-bearing: PXE observation (ObservePXE matches
		// hardware->'nics') and netboot MAC arming (macsFor) key on it,
		// and it doubles as the spec view's disk inventory.
		if hw := hardwareFromReport(pending.Report); hw != nil {
			raw, merr := json.Marshal(hw)
			if merr == nil {
				if err := s.Machines.SetHardware(ctx, m.ID, raw); err != nil {
					obs.FromContext(ctx).WarnContext(ctx, "claim: hardware projection failed",
						obs.FieldMachineID, m.ID, "err", err.Error())
				}
			}
		}
		if _, err := s.Machines.SaveLayout(ctx, m.ID, "ramdisk", pending.Report, 0); err != nil {
			obs.FromContext(ctx).WarnContext(ctx, "claim: probe report migration failed",
				obs.FieldMachineID, m.ID, "err", err.Error())
		} else {
			reported = true
		}
	}
	if reported {
		// The claim-with-report IS the headless machine's discovery
		// (docs/13-headless-design.md §3.1): the probe report just became
		// the spec view — settle at the same point the ramdisk probe does
		// (probe.go sets ready when the layout snapshot lands). protocol=
		// none has no controller for the discover flow to probe, so
		// without this a claimed headless machine sits in registering
		// forever (real-hardware finding, office-PC enroll→claim round
		// 2026-11-12). A firmware-only claim keeps registering: zero
		// facts is not a ready machine.
		if err := s.Machines.SetState(ctx, m.ID, "ready"); err != nil {
			obs.FromContext(ctx).WarnContext(ctx, "claim: state settle failed",
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
