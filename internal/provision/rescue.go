package provision

// The rescue flow (docs/07-bmc.md §6.5, docs/09-roadmap.md 下一阶段 11):
// boot_rescue boots the machine into the engine's in-memory alpine rescue
// system — the agent runtime with a rescue plan (sshd + the operator's
// keys, NO install steps). Dual carrier, the install pathway's own split:
//
//	virtual_media — the alpine standard ISO repacked with the agent overlay
//	                  + rescue plan at the media root (dd's composition); the
//	                  extended pool serves sshd over alpine_repo.
//	pxe           — the alpine NETBOOT tarball tree (BuildAgentNetboot) +
//	                  per-MAC registry entries; the plan is fetched via
//	                  mammoth_base (the /render/{token} face, context
//	                  answers). Carrier default: BMC machines mount media,
//	                  protocol=none machines PXE (headless gate lifted —
//	                  this is the rescue they can actually get); an explicit
//	                  boot:"pxe" overrides for vMedia-dead firmware.
//
// One composite stage: build the carrier, arm (the strategy layer's own
// sequences — headless WoL/evidence included), wait for the agent's
// rescue-ready report, release the payload. The machine STAYS in the RAM
// OS — the operator repairs it over SSH or the console, then any reboot
// returns it to the installed system.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/builder"
	"github.com/3th1nk/mammoth/internal/obs"
	rescuerender "github.com/3th1nk/mammoth/internal/render/rescue"
	"github.com/3th1nk/mammoth/internal/store"
)

// rescuePlanName is the plan file's name on the media root (vMedia) and in
// the context answers (PXE fetch) — the agent's find_plan picks both.
const rescuePlanName = "agent-plan.sh"

// rescueKernelArgs boots the agent from the repacked alpine ISO — the dd
// ISO line's shape (render/dd isoKernelArgs).
// ttyS0 FIRST, tty0 LAST (the /dev/console primary — VGA stays live
// through the openrc handoff; 2288H 2026-11-08 ttyS0-last froze the KVM
// view at the initramfs tail while the system ran on SOL).
const rescueKernelArgs = "modules=loop,squashfs,sd-mod,usb-storage,ext4,vfat console=ttyS0,115200 console=tty0"

// rescueTaskContext is the rescue flow's context marker: the media facts
// (token / media_uri / boot_strategy / netboot) share the install context's
// JSON keys on purpose — the strategy arms, the shared release paths and
// FetchAnswerFile parse them without knowing the flow.
type rescueTaskContext struct {
	Token    string `json:"token"`
	MediaURI string `json:"media_uri"`
}

// rescueRecord is the agent report's persisted shape (RecordRescueReady).
type rescueRecord struct {
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	Status  string     `json:"status,omitempty"`
	Detail  string     `json:"detail,omitempty"`
}

// rescueCarrierFor resolves the rescue boot carrier: headless machines have
// no media slot — PXE is the rescue they can get; an explicit boot:"pxe"
// overrides for vMedia-dead firmware; the default for BMC machines is the
// virtual-media mount. Invalid values are a schema error regardless of
// protocol (the headless override forces the carrier, never a typo).
func rescueCarrierFor(protocol, boot string) (string, error) {
	switch boot {
	case "", "pxe", "virtual_media":
	default:
		return "", classifiedErr("SCHEMA_INVALID_ACTION", false,
			"boot_rescue.boot must be \"pxe\" or \"virtual_media\", got %q", boot)
	}
	if protocol == "none" {
		return "pxe", nil // no media slot — PXE is the rescue headless gets
	}
	if boot == "" {
		return "virtual_media", nil
	}
	return boot, nil
}

func parseRescueContext(task *store.Task) rescueTaskContext {
	var rctx rescueTaskContext
	_ = json.Unmarshal(task.Context, &rctx)
	return rctx
}

func parseRescueRecord(task *store.Task) *rescueRecord {
	var ctx struct {
		Rescue *rescueRecord `json:"rescue"`
	}
	if json.Unmarshal(task.Context, &ctx) != nil {
		return nil
	}
	return ctx.Rescue
}

// runRescueStage is the rescue flow's single stage. Retry semantics: every
// entry re-checks the persisted report first (a retry after the agent
// already reported completes without rebuilding), then rebuilds the
// carrier idempotently (builds overwrite, registry upserts re-arm, the
// strategy arm is the same dance).
func (e *Executor) runRescueStage(ctx context.Context, task *store.Task, job *store.Job) error {
	a, err := decodeAction(job.Action)
	if err != nil {
		return err
	}
	m, merr := e.Machines.Get(ctx, task.MachineID)
	if merr != nil {
		return merr
	}
	// Carrier decision: headless machines have no media slot — PXE is the
	// rescue they can get; an explicit boot:"pxe" overrides for vMedia-dead
	// firmware; the default for BMC machines is the virtual-media mount.
	headless := m.BMCProtocol == "none"
	carrier, cerr := rescueCarrierFor(m.BMCProtocol, a.Boot)
	if cerr != nil {
		return cerr
	}
	a.Boot = carrier
	usePXE := a.Boot == "pxe"
	if usePXE && e.ProbeAlpineNetboot == "" {
		return classifiedErr("RESCUE_MEDIA_FAILED", false,
			"rescue boot=pxe needs the alpine NETBOOT tarball (set MAMMOTH_PROBE_ALPINE_NETBOOT) — the standard-ISO initramfs lacks the machine room's NIC drivers")
	}
	if !usePXE && e.ProbeAlpineISO == "" {
		return classifiedErr("RESCUE_MEDIA_FAILED", false,
			"rescue needs the alpine carrier ISO (set MAMMOTH_PROBE_ALPINE_ISO) — it is the boot media the BMC mounts")
	}
	if !headless {
		if _, _, _, ok := e.outOfBand(ctx, task); !ok {
			return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
		}
	}

	// Re-entry check: a report persisted before this attempt (retry after a
	// lost ack) is the terminal fact — release and complete.
	if rec := parseRescueRecord(task); rec != nil && rec.Status == "ok" {
		e.releaseRescueFresh(ctx, task, reasonCompleted)
		return nil
	}

	// Task token: reuse what a prior attempt minted, else mint + persist
	// (stage-entry CAS — rescue is single-stage, the index does not move).
	rctx := parseRescueContext(task)
	if rctx.Token == "" {
		token, terr := mintTaskToken()
		if terr != nil {
			return terr
		}
		if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
			"token": token,
		}); err != nil {
			return err
		}
		rctx.Token = token
	}
	external := strings.TrimSuffix(e.ExternalURL, "/")
	ictx := &installTaskContext{Token: rctx.Token}

	// The extended pool (sshd's apk source) is shared by both carriers; its
	// absence degrades to console-only rescue — an honest warning, not a
	// failure (the agent reports the same on the machine side).
	apksISO := ""
	if e.WindowsApplyAlpineISO != "" {
		extISO, terr := builder.EnsureISO(ctx, e.WindowsApplyAlpineISO, e.MediaDir)
		if terr != nil {
			obs.FromContext(ctx).WarnContext(ctx, "rescue extended pool fetch failed — console-only rescue",
				"err", terr.Error())
		} else {
			apksISO = extISO
		}
	}

	// The agent overlay + the rescue plan render once, before either
	// carrier consumes them: the overlay rides the tree (PXE) or the ISO
	// seed (vMedia); the plan rides the context answers (PXE fetch via
	// mammoth_base) and the ISO root (vMedia — find_plan scans the media
	// root).
	overlayName, overlay, oerr := builder.AgentOverlay()
	if oerr != nil {
		return classifiedErr("RESCUE_MEDIA_FAILED", true,
			"rescue agent overlay build failed: %s", oerr.Error())
	}
	plan := rescuerender.Render(rescuerender.Inputs{
		TaskToken:     rctx.Token,
		MachineID:     task.MachineID,
		CompleteURL:   external + "/render/" + rctx.Token + "/complete",
		AgentLogURL:   external + "/render/" + rctx.Token + "/agentlog",
		SSHPublicKeys: a.SshKeys,
	})
	seed := map[string]string{
		overlayName:    string(overlay),
		rescuePlanName: plan,
	}

	var kernelArgs string
	if usePXE {
		tarball, terr := builder.EnsureISO(ctx, e.ProbeAlpineNetboot, e.MediaWorkDir)
		if terr != nil {
			return classifiedErr("RESCUE_MEDIA_FAILED", true,
				"alpine netboot tarball fetch failed: %s", terr.Error())
		}
		tree, berr := builder.BuildAgentNetboot(ctx, builder.AgentNetbootOptions{
			TarballPath: tarball,
			ApksISOPath: apksISO,
			DestDir:     filepath.Join(e.BootTreeDir, rctx.Token),
			Overlay:     overlay,
		})
		if berr != nil {
			return classifiedErr("RESCUE_MEDIA_FAILED", true,
				"rescue boot tree build failed: %s", berr.Error())
		}
		files := external + "/netboot/files/" + rctx.Token
		args := "modules=loop,squashfs,sd-mod,usb-storage console=ttyS0,115200 console=tty0 ip=dhcp"
		args += " modloop=" + files + "/modloop"
		args += " apkovl=" + files + "/agent.apkovl.tar.gz"
		args += " mammoth_base=" + external + "/render/" + rctx.Token
		if apksISO != "" {
			args += " alpine_repo=" + files + "/apks"
		}
		var hw bmc.HardwareView
		_ = json.Unmarshal(m.Hardware, &hw)
		macs := macsFor(hw, nil)
		if len(macs) == 0 {
			return classifiedErr("NETBOOT_MAC_UNAVAILABLE", false,
				"machine %s has no NIC MAC in its inventory — PXE rescue arms entries by MAC", task.MachineID)
		}
		for _, mac := range macs {
			if err := e.Netboot.Upsert(ctx, &store.NetbootEntry{
				MAC: mac, TaskID: task.ID, MachineID: task.MachineID, Token: rctx.Token,
				Kind: "rescue", Kernel: tree.Kernel, Initrd: tree.Initrd,
				KernelArgs: bootifFor(args, mac), Extra: tree.Extra,
			}); err != nil {
				return classifiedErr("NETBOOT_REGISTER_FAILED", true,
					"netboot entry for %s: %s", mac, err.Error())
			}
		}
		ictx.BootStrategy = string(strategyPXE)
		ictx.Netboot = &netbootRecord{Token: rctx.Token, MACs: macs}
		kernelArgs = args
		e.Events.Append(ctx, "task", task.ID, "task.netboot_registered", map[string]any{
			"macs": macs, "kind": "rescue",
		})
	} else {
		alpineISO, terr := builder.EnsureISO(ctx, e.ProbeAlpineISO, e.MediaDir)
		if terr != nil {
			return classifiedErr("RESCUE_MEDIA_FAILED", true,
				"alpine carrier ISO fetch failed: %s", terr.Error())
		}
		mediaFile := filepath.Join("boot", fmt.Sprintf("boot-%s.iso", rctx.Token))
		_ = os.MkdirAll(filepath.Dir(filepath.Join(e.MediaDir, mediaFile)), 0o755)
		kernelArgs = rescueKernelArgs
		if apksISO != "" {
			if xerr := builder.ExtractIsoApks(ctx, "", apksISO, filepath.Join(e.BootTreeDir, rctx.Token, "apks")); xerr != nil {
				obs.FromContext(ctx).WarnContext(ctx, "rescue extended pool staging failed — console-only rescue",
					"err", xerr.Error())
				apksISO = "" // no staged pool → no alpine_repo on the boot line
			} else {
				kernelArgs += " alpine_repo=" + external + "/netboot/files/" + rctx.Token + "/apks"
			}
		}
		// Work/cache split mirrors the install strategy: the build scratch
		// lives in a per-task SUBPATH of MediaWorkDir (the builder RemoveAlls
		// its workDir — passing the mount point itself dies with "device or
		// resource busy"), then the finished ISO moves into the repo.
		outputPath := filepath.Join(e.MediaDir, mediaFile)
		buildWork := ""
		if e.MediaWorkDir != "" {
			buildWork = filepath.Join(e.MediaWorkDir, filepath.Base(mediaFile)+".build")
			outputPath = filepath.Join(e.MediaWorkDir, filepath.Base(mediaFile))
		}
		if berr := buildBootISO(ctx, alpineISO, outputPath, kernelArgs, seed, buildWork, e.MediaDir); berr != nil {
			return classifiedErr("RESCUE_MEDIA_FAILED", true,
				"rescue media build failed: %s", berr.Error())
		}
		if buildWork != "" {
			if merr := moveFile(outputPath, filepath.Join(e.MediaDir, mediaFile)); merr != nil {
				return classifiedErr("RESCUE_MEDIA_FAILED", true,
					"rescue media move into repo failed: %s", merr.Error())
			}
		}
		if e.MediaUploader != nil {
			if _, perr := e.MediaUploader.Push(ctx, filepath.Join(e.MediaDir, mediaFile), mediaFile); perr != nil {
				return classifiedErr("RESCUE_MEDIA_FAILED", true,
					"rescue media relay push failed: %s", perr.Error())
			}
		}
		ictx.BootStrategy = string(strategyVirtualMedia)
		ictx.MediaURI = mediaURIFor(e.MediaBaseURI, mediaFile)
		rctx.MediaURI = ictx.MediaURI
	}

	// One context patch carries every fact the machine face and the shared
	// paths need: token / boot_strategy / media_uri / netboot / answers
	// (FetchAnswerFile serves the plan at /render/{token}/agent-plan.sh;
	// releaseBootPayloadFresh routes by boot_strategy).
	patch := map[string]any{
		"token":         rctx.Token,
		"boot_strategy": ictx.BootStrategy,
		"answers":       ictx.Answers,
		"kernel_args":   kernelArgs,
	}
	if ictx.MediaURI != "" {
		patch["media_uri"] = ictx.MediaURI
	}
	if ictx.Netboot != nil {
		patch["netboot"] = ictx.Netboot
	}
	if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, patch); err != nil {
		return err
	}

	// ── arm via the strategy layer (headless WoL/evidence included) ─────
	session := &bootSession{Task: task, Job: job, Ictx: ictx}
	if usePXE {
		if err := e.pxe().arm(ctx, session); err != nil {
			return err
		}
	} else if err := e.virtualMedia().arm(ctx, session); err != nil {
		return err
	}
	e.Events.Append(ctx, "task", task.ID, "task.stage_changed", map[string]any{
		"stage": "rescue", "detail": "rescue carrier armed — waiting for the agent's rescue-ready report",
	})
	obs.FromContext(ctx).InfoContext(ctx, "rescue carrier armed, waiting for report",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID, "carrier", a.Boot)

	// ── wait for the rescue-ready report (the callback persists it under
	// the context's "rescue" key) ────────────────────────────────────────
	// The rescue budget is its own default (25m), NOT the ramdisk probe's
	// 10m: the rescue boot chain carries bootstrap_tools' apk pull from
	// the extended pool (sfdisk/util-linux/...) before sshd and the
	// report — real-hardware (.250/2288H, 2026-11-07): POST + PXE + modloop
	// + bootstrap ≈ 8-12 minutes on its own, and a 10m budget killed the
	// agent mid-bootstrap so every retry cycle restarted the boot from
	// zero (the KVM-observed double-boot). ProbeWait still overrides when
	// the deployment sets it.
	wait := e.RescueWait
	if wait <= 0 {
		wait = 25 * time.Minute
	}
	if e.ProbeWait > 0 {
		wait = e.ProbeWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			e.releaseRescueFresh(context.WithoutCancel(ctx), task, "canceled")
			return ctx.Err()
		case <-deadline.C:
			// No report: the machine never reached the rescue system (or
			// the report path is broken). Release; BMC machines go down so
			// a retry starts clean, headless machines lean on the manual
			// fallback (the same discipline as the boot evidence wait).
			cctx := context.WithoutCancel(ctx)
			e.releaseRescueFresh(cctx, task, "timeout")
			e.restoreBootOrderBestEffort(cctx, task, "rescue timeout")
			if !headless {
				if c2, a2, p2, ok2 := e.outOfBand(cctx, task); ok2 {
					_, _ = e.BMC.Do(cctx, a2, c2, p2, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
						return nil, d.SetPower(ctx, a2, c2, bmc.PowerOff)
					})
				}
			}
			return classifiedErr("RESCUE_TIMEOUT", true,
				"no rescue-ready report within %s — carrier released%s", wait.String(),
				map[bool]string{true: "", false: ", machine powered off"}[headless])
		case <-tick.C:
			// Cooperative cancel: the runner's cancel check lives at stage
			// boundaries, which this long wait never reaches (real-hardware:
			// a cancel request sat unanswered through the full 10-minute
			// budget). Same in-loop check the install_os wait runs — the
			// ErrCanceled return makes the runner's finish path run the
			// release/compensation.
			if cancelReq, cerr := e.Jobs.IsCancelRequested(ctx, task.ID); cerr == nil && cancelReq {
				e.releaseRescueFresh(context.WithoutCancel(ctx), task, "canceled")
				return ErrCanceled
			}
			fresh, ferr := e.Jobs.GetTask(ctx, task.ID)
			if ferr != nil {
				continue // transient read failure — the next tick retries
			}
			rec := parseRescueRecord(fresh)
			if rec == nil {
				continue
			}
			switch rec.Status {
			case "ok":
				e.releaseRescueFresh(ctx, task, reasonCompleted)
				e.restoreBootOrderBestEffort(ctx, task, "rescue ready")
				e.Events.Append(ctx, "task", task.ID, "task.boot_rescue", map[string]any{
					"detail":   rec.Detail,
					"carrier":  a.Boot,
					"ssh_keys": len(a.SshKeys),
				})
				obs.FromContext(ctx).InfoContext(ctx, "rescue ready — machine in the in-memory repair system",
					obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)
				return nil
			case "failed":
				return classifiedErr("RESCUE_AGENT_FAILED", false,
					"rescue agent reported failure: %s", rec.Detail)
			default:
				continue
			}
		}
	}
}

// restoreBootOrderBestEffort points the firmware back at disk (BMC
// machines; headless machines have nothing to restore). The one-shot CDROM/
// PXE override is consumed by the rescue boot, but a machine re-armed
// through firmware defaults must return to its installed system on the
// operator's reboot.
func (e *Executor) restoreBootOrderBestEffort(ctx context.Context, task *store.Task, why string) {
	if m, merr := e.Machines.Get(ctx, task.MachineID); merr != nil || m.BMCProtocol == "none" {
		return
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootDisk, false)
	}); err != nil {
		obs.FromContext(ctx).WarnContext(ctx, "boot-order restore failed (continuing)",
			obs.FieldTaskID, task.ID, "why", why, "err", err.Error())
	}
}

// releaseRescueFresh re-reads the task before the release (the claim-time
// snapshot predates the context patch — the same discipline as the install
// path's releaseBootPayloadFresh, which this reuses: boot_strategy routes
// the strategy's own release, ejecting on the completed path and reclaiming
// files on terminal paths).
func (e *Executor) releaseRescueFresh(ctx context.Context, task *store.Task, reason string) {
	e.releaseBootPayloadFresh(ctx, task, reason)
}
