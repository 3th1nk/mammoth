package provision

// The decommission flow (docs/07-bmc.md §6.6, docs/09-roadmap.md 下一阶段
// 18): the machine lifecycle's terminal orchestration — every drive NIST
// 800-88 erased, the evidence captured (what was erased with which
// controller mechanism, what the machine looked like at retirement), then
// the retired terminal state. Pure capability composition: DriveEraser /
// FirmwareInventoryProvider / SELReader / BiosSetter reads all exist — this
// file is the编排 (orchestration) and the evidence record.
//
// Reinstatement is a fresh discover: the probe's own state machine walks
// the machine discovering → ready (no new endpoint, evidence-driven — a
// machine worth reinstating is a machine worth re-probing).
//
// Cancel semantics: erase is not rollbackable — a cancel after the erase
// stage started leaves the disks erased (documented; the submission's
// confirm flag is the one gate, there is no second).

import (
	"context"
	"encoding/json"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

func (e *Executor) runDecommissionStage(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	stage := StageNames(FlowDecommission)[seq]
	ctx = obs.With(ctx, "stage", stage)
	switch stage {
	case "erase_drives":
		return e.decommissionErase(ctx, task)
	case "evidence":
		return e.decommissionEvidence(ctx, task)
	case "retire":
		return e.decommissionRetire(ctx, task)
	default:
		return classifiedErr("JOB_UNKNOWN_STAGE", false, "unknown decommission stage %s", stage)
	}
}

// decommissionErase erases EVERY drive the controller reports (the NIST
// 800-88 sanitization intent — a retiring machine keeps nothing). The
// runner's live-table validation runs inside eraseDrives: serials resolved
// fresh, the task.drive_erase event carries the erased serials and the
// controller's own reported mechanisms. Missing capability is an honest
// terminal failure — a machine whose drives cannot be erased is NOT retired.
func (e *Executor) decommissionErase(ctx context.Context, task *store.Task) error {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}
	obs.FromContext(ctx).InfoContext(ctx, "decommission erase starting (every drive)",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)
	rec, err := e.eraseDrives(ctx, task, addr, cred, proto, nil, true)
	if err != nil {
		return err
	}
	// The durable copy rides the task context (the event is the event face;
	// the evidence stage persists it into the retirement record).
	if perr := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
		"erase": rec,
	}); perr != nil {
		return perr
	}
	return nil
}

// decommissionEvidence captures the retirement record: firmware inventory,
// SEL tail, BIOS attribute snapshot — each best-effort (a capability-absent
// controller degrades the evidence, never fails the flow; the drives are
// already erased and the retirement is happening either way). The machine's
// identity facts come from the row. One JSON blob per task, rolling-window
// persisted (acceptance_reports' discipline).
func (e *Executor) decommissionEvidence(ctx context.Context, task *store.Task) error {
	// Context mutates between stages — the erase record lands via
	// PatchTaskContext while erase_drives runs; re-enter from the fresh
	// record (verify_ready's re-read discipline).
	fresh, ferr := e.Jobs.GetTask(ctx, task.ID)
	if ferr != nil {
		return ferr
	}
	task = fresh
	machine, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	ev := map[string]any{
		"machine": map[string]any{
			"id":     machine.ID,
			"vendor": machine.Vendor, "model": machine.Model,
			"serial": machine.SerialNumber,
			"bmc":    machine.BMCAddress,
		},
	}

	// The erase record rides the task context: eraseDrives appends the
	// serials + mechanisms into the action result (task.drive_erase event
	// is the event face; the context carries the durable copy).
	var actx struct {
		Erase json.RawMessage `json:"erase"`
	}
	_ = json.Unmarshal(task.Context, &actx)
	if len(actx.Erase) > 0 {
		ev["erase"] = json.RawMessage(actx.Erase)
	}

	// Firmware inventory (best-effort — the controller may lack the
	// capability; "unknown firmware" is honest evidence too).
	if res, err := e.BMC.Do(ctx, addr, cred, proto, "firmware_inventory", func(ctx context.Context, d bmc.Driver) (any, error) {
		fp, ok := d.(bmc.FirmwareInventoryProvider)
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "firmware_inventory"}
		}
		return fp.FirmwareInventory(ctx, addr, cred)
	}); err == nil {
		ev["firmware"] = res
	} else {
		ev["firmware"] = map[string]any{"available": false}
	}

	// SEL tail (best-effort, the rescue-room record belongs with the
	// retirement paperwork).
	if res, err := e.BMC.Do(ctx, addr, cred, proto, "sel", func(ctx context.Context, d bmc.Driver) (any, error) {
		sr, ok := d.(bmc.SELReader)
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "sel"}
		}
		return sr.SystemEventLog(ctx, addr, cred)
	}); err == nil {
		ev["sel"] = res
	} else {
		ev["sel"] = map[string]any{"available": false}
	}

	// BIOS attribute snapshot (best-effort — paired with the firmware
	// baseline question an asset auditor will ask next).
	if res, err := e.BMC.Do(ctx, addr, cred, proto, "bios_attributes", func(ctx context.Context, d bmc.Driver) (any, error) {
		bs, ok := d.(bmc.BiosSetter)
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "bios_attributes"}
		}
		return bs.BiosAttributes(ctx, addr, cred)
	}); err == nil {
		ev["bios"] = res
	} else {
		ev["bios"] = map[string]any{"available": false}
	}

	raw, _ := json.Marshal(ev)
	rep := &store.DecommissionReport{
		MachineID: task.MachineID, TaskID: task.ID,
		Content: json.RawMessage(raw),
	}
	if serr := e.Machines.SaveDecommissionReport(ctx, rep); serr != nil {
		obs.FromContext(ctx).WarnContext(ctx, "decommission report save failed (continuing)",
			"err", serr.Error())
	}
	e.Events.Append(ctx, "task", task.ID, "task.decommission_evidence", map[string]any{
		"report_id": rep.ID,
	})
	obs.FromContext(ctx).InfoContext(ctx, "decommission evidence captured",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)
	return nil
}

// decommissionRetire flips the machine to its terminal state. Reinstatement
// is a fresh discover (the probe's state machine walks it back to ready) —
// documented, no endpoint.
func (e *Executor) decommissionRetire(ctx context.Context, task *store.Task) error {
	if err := e.Machines.SetState(ctx, task.MachineID, "retired"); err != nil {
		return err
	}
	e.Events.Append(ctx, "machine", task.MachineID, "machine.retired", map[string]any{
		"task_id": task.ID,
	})
	obs.FromContext(ctx).InfoContext(ctx, "machine retired",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)
	return nil
}
