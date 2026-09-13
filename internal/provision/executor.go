package provision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/bmc/compat"
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/render"
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
	Inband      *inbandssh.Collector
	Render      *render.Registry
	// ExternalURL is the base address machines reach for answer files.
	ExternalURL string
	// MediaDir is the local media repository (boot ISOs land here).
	MediaDir string
	// MediaNFSBase is the NFS URI base the BMC uses to fetch media.
	MediaNFSBase string
	// BootSettleDelay waits between media mount and power-on — covers
	// out-of-band media transfer tails (NFS relay pushes).
	BootSettleDelay time.Duration
	// MediaWorkDir is the scratch directory for media builds — use when the
	// media repo lives on a size-limited share (the extract+assemble needs
	// ~2x the image size transient).
	MediaWorkDir string
	// MediaBaseURI is the BMC-reachable media base URI the BMC mounts from
	// (nfs://, cifs://, ftp:// — the firmware decides what it accepts).
	MediaBaseURI string
	// MediaUploader, when configured, moves the assembled boot ISO into the
	// BMC-reachable share in-process (SSH relay today; FTP/HTTP relays slot
	// in behind the same shape). Deployments that mount the export directly
	// need none.
	MediaUploader MediaUploader
	// RamdiskEnabled declares the optional ramdisk probe feature
	// (docs/05-inventory.md §4 — virtual media carrier, V1 alpine).
	RamdiskEnabled bool
	// ProbeAlpineISO is the alpine standard ISO (local path or URL) the
	// probe medium is built from — the lts kernel+modloop carry the
	// real-server storage drivers the probe exists to see.
	ProbeAlpineISO string
	// ProbeStaticCIDR, when set, is the probe's DHCP fallback address for
	// machines WITHOUT ssh.address ("198.51.100.75/24") — machine rooms
	// without DHCP. Empty disables the fallback.
	ProbeStaticCIDR string
	// ProbePrefix is the prefix length applied to a machine's bare
	// ssh.address when building the DHCP-fallback CIDR (default 24).
	ProbePrefix int
	// ProbeGateway, when set, becomes the static fallback's default route —
	// needed when the report URL is in a different subnet than the machine.
	ProbeGateway string
	// ProbeWait bounds the ramdisk discover's wait for the machine's report
	// (boot + scan + report ≈ 1 minute in qemu; real BMC virtual media is
	// slower). Zero applies the default (10m).
	ProbeWait time.Duration

	// LayoutKeep is the per-machine snapshot retention (docs/08-data-model.md).
	LayoutKeep int

	// Netboot is the network-boot registry behind the pxe boot strategy
	// (docs/06-install-pipeline.md §3.3). Nil disables pxe — a pxe task on
	// a runner without the service fails fast with NETBOOT_UNAVAILABLE.
	Netboot *store.NetbootRepo
	// BootTreeDir is the per-task netboot payload root (MediaDir/netboot):
	// ExtractBootFiles writes trees here, the netboot service serves them.
	BootTreeDir string
	// BootStrategyDefault names the carrier used when a spec does not
	// declare boot.strategy ("virtual_media" unless the deployment opts in).
	BootStrategyDefault string

	// VerifyReadyWait bounds verify_ready's in-band poll for the new system
	// after the completion report (reboot + POST + sshd takes minutes —
	// real-hardware finding; task-level retries cover only ~30s). Zero
	// applies the default (10m).
	VerifyReadyWait time.Duration
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
		stage := StageNames(task.FlowName)[seq]
		err := e.runInstallStage(ctx, task, job, seq)
		// Terminal failure after the payload was produced: reclaim it now
		// (retryable failures deliberately keep it — the retry's
		// prepare_media rebuilds it anyway).
		if err != nil && installMediaProduced(stage) && !Classified(err).Retryable {
			e.releaseBootPayload(ctx, task, parseInstallContext(task), "terminal failure")
		}
		return err
	default:
		return classifiedErr("JOB_UNKNOWN_FLOW", false, "unknown flow %s", task.FlowName)
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
	img := bmc.MediaImage{URL: a.ImageURL, Kind: bmc.MediaBoot}
	if _, err := e.BMC.Do(mctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.EjectMedia(ctx, addr, cred, img)
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
			return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown power action %s", a.Type)
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
			return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown boot device %s", a.Device)
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

	case "eject_media":
		_, err = e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.EjectMedia(ctx, addr, cred, bmc.MediaImage{URL: a.ImageURL})
		})
		return err

	case "discover":
		return e.runDiscover(ctx, task, job)

	default:
		return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown action type %s", a.Type)
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

// runDiscover is the spec-level probe (docs/05-inventory.md §2) plus, under
// `probe: auto`, the partition-level layout snapshot (docs/05-inventory.md §1
// combination logic, M2). Machine lifecycle follows registering → discovering
// → ready | error; failures land on machine.last_error with classified codes.
func (e *Executor) runDiscover(ctx context.Context, task *store.Task, job *store.Job) error {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	probeKind := "auto"
	if job != nil {
		if a, err := decodeAction(job.Action); err == nil && a.Probe != "" {
			probeKind = a.Probe
		}
	}

	// Explicit inband_ssh: partition-level only, no BMC round trip.
	if probeKind == "inband_ssh" {
		return e.collectLayout(ctx, task, true)
	}
	if probeKind == "ramdisk" {
		if !e.RamdiskEnabled {
			return classifiedErr("BMC_UNSUPPORTED", false,
				"ramdisk probe is optional and disabled (set MAMMOTH_RAMDISK_ENABLED); it also needs the alpine carrier ISO (MAMMOTH_PROBE_ALPINE_ISO)")
		}
		return e.probeRamdisk(ctx, task)
	}
	if probeKind != "auto" && probeKind != "redfish" {
		return classifiedErr("SCHEMA_INVALID_ACTION", false, "unknown probe kind %s", probeKind)
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

	// auto: the spec view is complete; when SSH access is configured the
	// layout snapshot is part of the discovery intent. An in-band failure is
	// an explicit classified error (never a hang) while the machine keeps
	// its ready spec view (docs/05-inventory.md §1, §3).
	if probeKind == "auto" {
		return e.collectLayout(ctx, task, false)
	}
	return nil
}

// collectLayout runs the inband_ssh probe and persists the snapshot. With
// required=true (explicit probe action) a missing configuration is an error;
// under auto it silently skips when SSH access was never configured.
func (e *Executor) collectLayout(ctx context.Context, task *store.Task, required bool) error {
	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}
	if m.SSHCredentialID == nil || m.SSHAddress == "" {
		if required {
			return classifiedErr("SCHEMA_SSH_NOT_CONFIGURED", false,
				"inband_ssh requires ssh_credential_id and ssh.address on the machine")
		}
		return nil
	}
	credRow, err := e.Credentials.Get(ctx, *m.SSHCredentialID)
	if err != nil {
		return classifiedErr("SCHEMA_UNKNOWN_CREDENTIAL", false, "ssh credential %q not found", *m.SSHCredentialID)
	}
	if credRow.Type != "ssh" {
		return classifiedErr("SCHEMA_INVALID_CREDENTIAL", false, "credential %q is not an ssh credential", *m.SSHCredentialID)
	}
	plain, err := e.Crypto.Decrypt(credRow.SecretEncrypted)
	if err != nil {
		return classifiedErr("CREDENTIAL_DECRYPT_FAILED", false, "credential decrypt failed")
	}
	var secret struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(plain, &secret); err != nil {
		return classifiedErr("CREDENTIAL_DECRYPT_FAILED", false, "credential payload malformed")
	}

	_, span := obs.Tracer().Start(ctx, "probe.inband_ssh")
	defer span.End()

	res, err := e.Inband.Collect(ctx, m.SSHAddress, inbandssh.Credentials{
		Username: secret.Username, Password: secret.Password, PrivateKey: secret.PrivateKey,
	})
	if err != nil {
		return e.inbandFailed(ctx, task, err)
	}

	version, err := e.Machines.SaveLayout(ctx, task.MachineID, res.Layout.Source, marshalJSON(res.Layout), e.LayoutKeep)
	if err != nil {
		return err
	}
	e.refreshNICs(ctx, task.MachineID, res.NICs)
	e.Events.Append(ctx, "machine", task.MachineID, "machine.layout_captured", map[string]any{
		"version": version, "source": res.Layout.Source, "disks": len(res.Layout.Disks),
	})
	obs.FromContext(ctx).InfoContext(ctx, "layout snapshot captured",
		obs.FieldMachineID, task.MachineID, "version", version, "disks", len(res.Layout.Disks))
	return nil
}

// inbandFailed surfaces in-band failures explicitly: the machine keeps its
// ready spec view, the classified code lands on last_error, and the task
// fails (retryable where the condition may clear).
func (e *Executor) inbandFailed(ctx context.Context, task *store.Task, cause error) error {
	ei := store.ErrorInfo{Code: "NETWORK_UNREACHABLE", Message: cause.Error(), Retryable: true}
	var ie *inbandssh.Error
	if errors.As(cause, &ie) {
		ei = store.ErrorInfo{Code: ie.Code, Message: ie.Error(), Retryable: ie.Retryable}
	}
	// state stays "ready": the spec view already captured is valid.
	_ = e.Machines.SetError(ctx, task.MachineID, "ready", &ei)
	return &errInfo{ei}
}

// refreshNICs merges link facts (MAC join) into the stored hardware view
// (docs/05-inventory.md §3: NIC link info refreshes alongside layout).
func (e *Executor) refreshNICs(ctx context.Context, machineID string, facts []inbandssh.NICFacts) {
	if len(facts) == 0 {
		return
	}
	m, err := e.Machines.Get(ctx, machineID)
	if err != nil || len(m.Hardware) == 0 {
		return
	}
	var hw bmc.HardwareView
	if json.Unmarshal(m.Hardware, &hw) != nil {
		return
	}
	byMAC := map[string]inbandssh.NICFacts{}
	for _, f := range facts {
		if f.MAC != "" {
			byMAC[strings.ToLower(f.MAC)] = f
		}
	}
	changed := false
	for i := range hw.NICs {
		if f, ok := byMAC[strings.ToLower(hw.NICs[i].MAC)]; ok {
			hw.NICs[i].LinkUp = f.LinkUp
			changed = true
		}
	}
	if !changed {
		return
	}
	_ = e.Machines.SetHardware(ctx, machineID, marshalJSON(hw))
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
