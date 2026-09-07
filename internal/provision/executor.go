package provision

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/bmc/compat"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// Executor performs the current stage of a claimed task. Stage Do() bodies
// are idempotent-safe: retries re-enter them (docs/02-architecture.md §2.3) —
// for out-of-band actions this means re-issuing a power/boot/media call,
// which BMCs treat as convergent.
type Executor struct {
	Machines    *store.MachineRepo
	Credentials *store.CredentialRepo
	Jobs        *store.JobRepo
	Events      *store.EventRepo
	Crypto      *store.SecretCrypto
	BMC         *bmc.Registry
	Compat      *compat.Registry
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
		return e.runDiscover(ctx, task, job)
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
		return e.runDiscover(ctx, task, job)

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

// runDiscover is the spec-level probe (docs/05-inventory.md §2, M1): it
// verifies BMC connectivity and credentials, backfills identity, and collects
// the hardware view. Machine lifecycle follows registering → discovering →
// ready | error; failures land on machine.last_error with classified BMC codes.
func (e *Executor) runDiscover(ctx context.Context, task *store.Task, job *store.Job) error {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	// Action-declared probe overrides the machine default (docs/03-api.md §2).
	if job != nil {
		if a, err := decodeAction(job.Action); err == nil && a.Probe != "" && a.Probe != "auto" {
			if a.Probe == "inband_ssh" {
				return classifiedErr("BMC_UNSUPPORTED", false,
					"inband_ssh probe arrives with M2; use redfish or auto")
			}
			proto = bmc.Protocol(a.Probe)
		}
	}

	_ = e.Machines.SetState(ctx, task.MachineID, "discovering")

	res, err := e.BMC.Do(ctx, addr, cred, proto, "probe", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.Probe(ctx, addr, cred)
	})
	if err != nil {
		return e.discoverFailed(ctx, task, err)
	}
	info := res.(bmc.BMCInfo)

	// Hardware view: redfish collects the spec-level view; drivers without
	// inventory support (ipmi) leave hardware empty rather than failing —
	// the BMC answered, so identity still lands.
	var hardware bmc.HardwareView
	hw, herr := e.BMC.Do(ctx, addr, cred, proto, "collect_inventory", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.CollectInventory(ctx, addr, cred)
	})
	switch {
	case herr == nil:
		hardware = hw.(bmc.HardwareView)
	case bmcKind(herr) == bmc.KindUnsupported:
		// identity-only machine (e.g. ipmi-only): coverage notes stay empty
	default:
		return e.discoverFailed(ctx, task, herr)
	}

	// Known blind spots from the vendor matrix downgrade coverage (docs/07-bmc.md §4).
	for _, note := range e.Compat.InventoryNotes(info.Vendor, info.Model) {
		hardware.Note(note)
	}

	err = e.Machines.UpdateProbeResult(ctx, task.MachineID, store.ProbeResult{
		Vendor:          strPtr(info.Vendor),
		Model:           strPtr(info.Model),
		SerialNumber:    strPtr(info.SerialNumber),
		FirmwareVersion: strPtr(info.FirmwareVersion),
		Hardware:        marshalJSON(hardware),
		PowerState:      string(info.PowerState),
		State:           "ready",
	})
	if err != nil {
		return err
	}
	e.Events.Append(ctx, "machine", task.MachineID, "machine.discovered", map[string]any{
		"vendor": info.Vendor, "model": info.Model, "firmware": info.FirmwareVersion,
		"coverage": hardware.Coverage,
	})
	return nil
}

// discoverFailed records the classified failure on the machine (acceptance:
// "BMC credential errors get classified error codes") and fails the stage.
func (e *Executor) discoverFailed(ctx context.Context, task *store.Task, cause error) error {
	ei := Classified(cause)
	_ = e.Machines.SetError(ctx, task.MachineID, "error", &ei)
	return cause
}

// bmcKind extracts the error kind from a possibly-wrapped bmc error.
func bmcKind(err error) bmc.ErrorKind {
	var be *bmc.Error
	if errors.As(err, &be) {
		return be.Kind
	}
	return ""
}

func marshalJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
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
