package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/builder"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// mintProbeToken generates the machine-face credential when job creation
// left none (defensive — the actions API mints discover tokens at creation).
func mintProbeToken() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// withPrefixLen normalizes an address into CIDR form: an address that
// already carries a prefix passes through; a bare IP gets the given prefix
// length. Empty input yields ("", false).
func withPrefixLen(addr string, prefix int) (string, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", false
	}
	if strings.Contains(addr, "/") {
		return addr, true
	}
	if prefix <= 0 || prefix > 128 {
		prefix = 24
	}
	return fmt.Sprintf("%s/%d", addr, prefix), true
}

// Ramdisk probe (docs/05-inventory.md §4, V1 alpine carrier): build the
// task's probe payload — a repacked ISO mounted over virtual media, or a
// network boot tree served by the netboot service — boot the machine into
// it once, and wait for the machine-face report endpoint to land a layout
// snapshot (source=ramdisk). The probe environment powers the machine off
// when its report is delivered; this side compensates the carrier either
// way.
//
// The token mechanism mirrors the install flow: the unguessable task token
// in the report URL is the machine's credential (context->>'token'), minted
// and persisted before any media moves so a retried task reuses it.
func (e *Executor) probeRamdisk(ctx context.Context, task *store.Task, usePXE bool) error {
	if e.ProbeAlpineISO == "" {
		return classifiedErr("BMC_UNSUPPORTED", false,
			"ramdisk probe needs the alpine carrier ISO (set MAMMOTH_PROBE_ALPINE_ISO)")
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}
	_ = e.Machines.SetState(ctx, task.MachineID, "discovering")

	// Baseline BEFORE boot: the poll waits for a snapshot strictly newer.
	var baseline time.Time
	if _, captured, err := e.Machines.LatestLayoutBySource(ctx, task.MachineID, "ramdisk"); err == nil {
		baseline = captured
	}

	// Task token: reuse what a prior attempt minted, else mint + persist
	// (stage-entry CAS — discover is single-stage, the index does not move).
	var pctx struct {
		Token    string `json:"token"`
		MediaURI string `json:"media_uri"`
	}
	_ = json.Unmarshal(task.Context, &pctx)
	if pctx.Token == "" {
		var err error
		pctx.Token, err = mintProbeToken()
		if err != nil {
			return err
		}
		if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
			"token": pctx.Token,
		}); err != nil {
			return err
		}
	}

	// Static network fallback: the machine's own ssh.address is the
	// authoritative source (mammoth reaches the machine over it, the report
	// travels the reverse path) — the global CIDR is the fallback for
	// machines without one. The gateway carries cross-subnet report targets.
	staticCIDR, gateway := e.ProbeStaticCIDR, e.ProbeGateway
	if m, merr := e.Machines.Get(ctx, task.MachineID); merr == nil && m.SSHAddress != "" {
		if c, ok := withPrefixLen(m.SSHAddress, e.ProbePrefix); ok {
			staticCIDR, gateway = c, e.ProbeGateway
		}
	}

	// Build the probe payload from the alpine carrier (local path or URL —
	// EnsureISO caches either). pxe extracts a boot tree and arms the
	// registry; virtual media repacks the probe ISO.
	mediaURI := ""
	var pxeMACs []string
	if usePXE {
		if e.ProbeAlpineNetboot == "" {
			return classifiedErr("BMC_UNSUPPORTED", false,
				"probe boot=pxe needs the alpine NETBOOT tarball (set MAMMOTH_PROBE_ALPINE_NETBOOT) — the standard-ISO initramfs usually lacks the machine room's NIC drivers, and without them the modloop can never be fetched")
		}
		if err := requireDiskHeadroom(e.BootTreeDir, 2<<30); err != nil {
			return classifiedErr("PROBE_MEDIA_FAILED", false, "%s", err.Error())
		}
		carrier, err := builder.EnsureISO(ctx, e.ProbeAlpineNetboot, e.MediaWorkDir)
		if err != nil {
			return classifiedErr("PROBE_MEDIA_FAILED", true, "netboot tarball unavailable: %s", err.Error())
		}
		carrierISO := ""
		if e.ProbeAlpineISO != "" {
			var ierr error
			carrierISO, ierr = builder.EnsureISO(ctx, e.ProbeAlpineISO, e.MediaWorkDir)
			if ierr != nil {
				return classifiedErr("PROBE_MEDIA_FAILED", true, "carrier ISO unavailable: %s", ierr.Error())
			}
		}
		tree, err := builder.BuildProbeNetboot(ctx, builder.ProbeNetbootOptions{
			TarballPath:   carrier,
			ApksISOPath:   carrierISO,
			DestDir:       filepath.Join(e.BootTreeDir, pctx.Token),
			ReportURL:     e.ExternalURL + "/render/" + pctx.Token + "/probe-report",
			StaticCIDR:    staticCIDR,
			StaticGateway: gateway,
			ModloopURL:    strings.TrimSuffix(e.ExternalURL, "/") + "/netboot/files/" + pctx.Token + "/modloop",
			ApksURL:       strings.TrimSuffix(e.ExternalURL, "/") + "/netboot/files/" + pctx.Token + "/apks",
		})
		if err != nil {
			return classifiedErr("PROBE_MEDIA_FAILED", true, "probe boot tree build failed: %s", err.Error())
		}
		m, merr := e.Machines.Get(ctx, task.MachineID)
		if merr != nil {
			return merr
		}
		var hw bmc.HardwareView
		_ = json.Unmarshal(m.Hardware, &hw)
		pxeMACs = macsFor(hw, nil)
		if len(pxeMACs) == 0 {
			return classifiedErr("NETBOOT_MAC_UNAVAILABLE", false,
				"machine %s has no NIC MAC in its inventory — the PXE probe arms entries by MAC", task.MachineID)
		}
		for _, mac := range pxeMACs {
			if err := e.Netboot.Upsert(ctx, &store.NetbootEntry{
				MAC: mac, TaskID: task.ID, MachineID: task.MachineID, Token: pctx.Token,
				Kind: "probe", Kernel: tree.Kernel, Initrd: tree.Initrd,
				KernelArgs: tree.KernelArgs, Extra: tree.Extra,
			}); err != nil {
				return classifiedErr("NETBOOT_REGISTER_FAILED", true,
					"netboot entry for %s: %s", mac, err.Error())
			}
		}
		if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
			"boot_strategy": string(strategyPXE),
			"netboot":       netbootRecord{Token: pctx.Token, MACs: pxeMACs},
		}); err != nil {
			return err
		}
		obs.FromContext(ctx).InfoContext(ctx, "probe boot tree armed", "macs", len(pxeMACs))
	} else {
		carrierISO, err := builder.EnsureISO(ctx, e.ProbeAlpineISO, e.MediaWorkDir)
		if err != nil {
			return classifiedErr("PROBE_MEDIA_FAILED", true, "carrier ISO unavailable: %s", err.Error())
		}
		mediaName := "probe-" + pctx.Token + ".iso"
		if _, err := builder.BuildProbeISO(ctx, builder.ProbeOptions{
			ISOPath:       carrierISO,
			OutputPath:    filepath.Join(e.MediaDir, mediaName),
			ReportURL:     e.ExternalURL + "/render/" + pctx.Token + "/probe-report",
			StaticCIDR:    staticCIDR,
			StaticGateway: gateway,
		}); err != nil {
			return classifiedErr("PROBE_MEDIA_FAILED", true, "probe ISO build failed: %s", err.Error())
		}
		mediaURI = mediaURIFor(e.MediaBaseURI, mediaName)
		if e.MediaUploader != nil {
			if _, perr := e.MediaUploader.Push(ctx, filepath.Join(e.MediaDir, mediaName)); perr != nil {
				return classifiedErr("PROBE_MEDIA_FAILED", true, "probe media relay push failed: %s", perr.Error())
			}
		}
		if mediaURI != pctx.MediaURI {
			if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
				"media_uri": mediaURI,
			}); err != nil {
				return err
			}
			pctx.MediaURI = mediaURI
		}
	}

	// Arm: one-shot PXE (no mount, no settle), or the virtual-media mount
	// dance (identical sequence to the install boot stage). The probe writes
	// no install-progress record — its context is not an install context.
	if usePXE {
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootPXE, true)
		}); err != nil {
			return err
		}
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "power_state", func(ctx context.Context, d bmc.Driver) (any, error) {
			return d.PowerState(ctx, addr, cred)
		}); err != nil {
			// observation only — the arm proceeds either way
		}
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetPower(ctx, addr, cred, bmc.Cycle)
		}); err != nil {
			return err
		}
	} else {
		media := bmc.MediaImage{URL: mediaURI, Kind: bmc.MediaBoot}
		_, _ = e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.EjectMedia(ctx, addr, cred, media)
		})
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.MountMedia(ctx, addr, cred, media)
		}); err != nil {
			return err
		}
		if settle := e.BootSettleDelay; settle > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settle):
			}
		}
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootCDROM, true)
		}); err != nil {
			return err
		}
		ps, perr := e.BMC.Do(ctx, addr, cred, proto, "power_state", func(ctx context.Context, d bmc.Driver) (any, error) {
			return d.PowerState(ctx, addr, cred)
		})
		action := bmc.PowerOn
		if perr == nil && ps.(bmc.PowerState) == bmc.PowerStateOn {
			action = bmc.Cycle
		}
		if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetPower(ctx, addr, cred, action)
		}); err != nil {
			return err
		}
	}
	e.Events.Append(ctx, "task", task.ID, "task.stage_changed", map[string]any{
		"stage": "discover", "detail": "probe media booted",
	})
	obs.FromContext(ctx).InfoContext(ctx, "probe media booted, waiting for report",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)

	// Wait for the report (the endpoint persists the snapshot; a probe that
	// fails to report drops to its diagnostic shell and the BMC SOL session
	// stays inspectable until the compensation below).
	wait := e.ProbeWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	compensate := func() {
		cctx := context.WithoutCancel(ctx)
		if usePXE {
			// The registry rows + boot tree go, and the machine lands back
			// on disk: restore boot order (the one-shot PXE override is
			// consumed, but a machine re-armed through firmware defaults
			// must not network-boot again), then power off like the VM
			// carrier always has.
			e.releasePXE(cctx, task.ID, pctx.Token, "completed")
			cred, addr, proto, ok := e.outOfBand(cctx, task)
			if ok {
				_, _ = e.BMC.Do(cctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
					return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootDisk, false)
				})
				_, _ = e.BMC.Do(cctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
					return nil, d.SetPower(ctx, addr, cred, bmc.PowerOff)
				})
			}
			return
		}
		media := bmc.MediaImage{URL: mediaURI, Kind: bmc.MediaBoot}
		e.probeCompensate(cctx, addr, cred, proto, media, "probe-"+pctx.Token+".iso")
	}
	for {
		select {
		case <-ctx.Done():
			compensate()
			return ctx.Err()
		case <-deadline.C:
			compensate()
			return classifiedErr("PROBE_TIMEOUT", true,
				"no probe report within %s — machine powered off, carrier released", wait.String())
		case <-tick.C:
			if _, captured, err := e.Machines.LatestLayoutBySource(ctx, task.MachineID, "ramdisk"); err == nil && captured.After(baseline) {
				compensate()
				obs.FromContext(ctx).InfoContext(ctx, "probe report received",
					obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)
				return nil
			}
		}
	}
}

// probeCompensate clears the probe footprint: eject the media, power the
// machine off, reclaim the ISO. Best effort throughout — the probe outcome
// is decided by the report, not by the cleanup.
func (e *Executor) probeCompensate(ctx context.Context, addr string, cred bmc.Credentials, proto bmc.Protocol, media bmc.MediaImage, mediaName string) {
	_, _ = e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.EjectMedia(ctx, addr, cred, media)
	})
	_, _ = e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetPower(ctx, addr, cred, bmc.PowerOff)
	})
	if e.MediaUploader != nil {
		_ = e.MediaUploader.Remove(ctx, mediaName)
	} else {
		_ = os.Remove(filepath.Join(e.MediaDir, mediaName))
	}
}
