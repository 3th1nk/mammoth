package provision

import (
	"context"
	"encoding/json"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// Executor performs the current stage of a claimed task. Stage Do() bodies
// are idempotent-safe: retries re-enter them (docs/02-architecture.md §2.3) —
// for M0's out-of-band actions this means re-issuing a power/boot/media call,
// which BMCs treat as convergent.
type Executor struct {
	Machines    *store.MachineRepo
	Credentials *store.CredentialRepo
	Jobs        *store.JobRepo
	Events      *store.EventRepo
	Crypto      *store.SecretCrypto
	BMC         *bmc.Registry
}

// ExecuteStage runs stage seq of the task's flow.
func (e *Executor) ExecuteStage(ctx context.Context, task *store.Task, job *store.Job, seq int) error {
	ctx, span := obs.Tracer().Start(ctx, "stage."+StageNames(task.FlowName)[seq])
	defer span.End()
	ctx = obs.With(ctx, obs.FieldStage, StageNames(task.FlowName)[seq],
		obs.FieldTaskID, task.ID, obs.FieldJobID, job.ID, obs.FieldMachineID, task.MachineID)

	switch task.FlowName {
	case FlowPower:
		return e.runPowerAction(ctx, task, job)
	case FlowDiscover:
		return e.runDiscover(ctx, task)
	case FlowInstall:
		return classifiedErr("INSTALL_NOT_IMPLEMENTED", false,
			"the install pipeline ships with M3; submit power/discover jobs meanwhile")
	default:
		return classifiedErr("JOB_UNKNOWN_FLOW", false, "unknown flow "+task.FlowName)
	}
}

// Compensate rolls back side effects on cancel (docs/02-architecture.md §2.4):
// M0's compensation is ejecting mounted virtual media.
func (e *Executor) Compensate(ctx context.Context, task *store.Task, job *store.Job) {
	if job.Type != "power" || len(job.Action) == 0 {
		return
	}
	a, err := decodeAction(job.Action)
	if err != nil || a.Type != "mount_media" {
		return
	}
	mctx := obs.With(ctx, obs.FieldStage, "compensate")
	cred, addr, proto, ok := e.outOfBand(mctx, task)
	if !ok {
		return
	}
	if _, err := e.BMC.Do(mctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.EjectMedia(ctx, addr, cred)
	}); err != nil {
		obs.FromContext(mctx).WarnContext(mctx, "compensation eject failed", "err", err.Error())
	}
}

// outOfBand resolves machine address + decrypted credentials + protocol.
func (e *Executor) outOfBand(ctx context.Context, task *store.Task) (bmc.Credentials, string, bmc.Protocol, bool) {
	machine, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return bmc.Credentials{}, "", "", false
	}
	credRow, err := e.Credentials.Get(ctx, machine.BMCCredentialID)
	if err != nil {
		return bmc.Credentials{}, "", "", false
	}
	plain, err := e.Crypto.Decrypt(credRow.SecretEncrypted)
	if err != nil {
		obs.FromContext(ctx).ErrorContext(ctx, "credential decrypt failed",
			obs.FieldMachineID, machine.ID, "err", err.Error())
		return bmc.Credentials{}, "", "", false
	}
	var secret struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(plain, &secret); err != nil {
		return bmc.Credentials{}, "", "", false
	}
	return bmc.Credentials{Username: secret.Username, Password: secret.Password},
		machine.BMCAddress, bmc.Protocol(machine.BMCProtocol), true
}

// runPowerAction executes the single generic BMC action of a power flow.
func (e *Executor) runPowerAction(ctx context.Context, task *store.Task, job *store.Job) error {
	a, err := decodeAction(job.Action)
	if err != nil {
		return err
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	switch a.Type {
	case "power_on", "power_off", "soft_off", "reboot", "hard_reboot", "cycle":
		act, ok := powerActionFor(a.Type)
		if !ok {
			return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown power action "+a.Type)
		}
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetPower(ctx, addr, cred, act)
		}); err != nil {
			return err
		}
		return e.refreshPowerState(ctx, task, addr, cred, proto)

	case "hard_reboot_alias_guard": // unreachable; keeps switch exhaustive
		return nil

	case "set_boot_device":
		dev, ok := bootDeviceFor(a.Device)
		if !ok {
			return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown boot device "+a.Device)
		}
		once := true
		if a.Once != nil {
			once = *a.Once
		}
		_, err = e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetBootDevice(ctx, addr, cred, dev, once)
		})
		return err

	case "mount_media":
		if a.ImageURL == "" {
			return classifiedErr("SCHEMA_INVALID_ACTION", false, "mount_media requires image_url")
		}
		_, err = e.BMC.Do(ctx, addr, cred, proto, "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.MountMedia(ctx, addr, cred, bmc.MediaImage{URL: a.ImageURL})
		})
		return err

	case "discover":
		return e.runDiscover(ctx, task)

	default:
		return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown action type "+a.Type)
	}
}

// refreshPowerState persists the observed state after a power action.
func (e *Executor) refreshPowerState(ctx context.Context, task *store.Task, addr string, cred bmc.Credentials, proto bmc.Protocol) error {
	res, err := e.BMC.Do(ctx, addr, cred, proto, "power_state", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.PowerState(ctx, addr, cred)
	})
	if err != nil {
		return nil // observation is best-effort; the action itself succeeded
	}
	if ps, ok := res.(bmc.PowerState); ok {
		return e.Machines.SetPowerState(ctx, task.MachineID, string(ps))
	}
	return nil
}

// runDiscover probes the BMC and backfills machine identity.
func (e *Executor) runDiscover(ctx context.Context, task *store.Task) error {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}
	res, err := e.BMC.Do(ctx, addr, cred, proto, "probe", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.Probe(ctx, addr, cred)
	})
	if err != nil {
		return err
	}
	info := res.(bmc.BMCInfo)
	vendor, model, serial, firmware := info.Vendor, info.Model, info.SerialNumber, info.FirmwareVersion
	err = e.Machines.UpdateProbeResult(ctx, task.MachineID, store.ProbeResult{
		Vendor:          strPtr(vendor),
		Model:           strPtr(model),
		SerialNumber:    strPtr(serial),
		FirmwareVersion: strPtr(firmware),
		PowerState:      string(info.PowerState),
		State:           "ready",
	})
	if err == nil {
		e.Events.Append(ctx, "machine", task.MachineID, "machine.discovered", map[string]any{
			"vendor": vendor, "model": model, "firmware": firmware,
		})
	}
	return err
}

func powerActionFor(apiType string) (bmc.PowerAction, bool) {
	switch apiType {
	case "power_on":
		return bmc.PowerOn, true
	case "power_off":
		return bmc.PowerOff, true
	case "soft_off":
		return bmc.SoftOff, true
	case "reboot":
		return bmc.SoftReboot, true
	case "hard_reboot":
		return bmc.HardReboot, true
	case "cycle":
		return bmc.Cycle, true
	default:
		return "", false
	}
}

func bootDeviceFor(d string) (bmc.BootDevice, bool) {
	switch d {
	case "pxe":
		return bmc.BootPXE, true
	case "disk":
		return bmc.BootDisk, true
	case "cdrom":
		return bmc.BootCDROM, true
	case "bios":
		return bmc.BootBIOS, true
	default:
		return "", false
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
