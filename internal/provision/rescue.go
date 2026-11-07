package provision

// The rescue flow (docs/07-bmc.md §6.4, docs/09-roadmap.md 下一阶段 11):
// boot_rescue boots the machine into the engine's in-memory alpine rescue
// system — the agent runtime with a rescue plan (sshd + the operator's
// keys, NO install steps), carried on the alpine standard ISO the same way
// the dd pathway carries it (overlay + plan ride the media root, the
// extended pool serves sshd over alpine_repo). One composite stage: build
// the carrier, boot, wait for the agent's rescue-ready report, release the
// payload. The machine STAYS in the RAM OS — the operator repairs it over
// SSH or the console, then any reboot returns it to the installed system.

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

// rescuePlanName is the ISO-root plan file the agent's find_plan picks up
// (the dd plan's sibling — same media-root convention, same loader).
const rescuePlanName = "agent-plan.sh"

// rescueKernelArgs boots the agent from the repacked alpine ISO — the dd
// ISO line's shape (render/dd isoKernelArgs) plus the staged extended pool
// when it exists (sshd is an apk from that pool; without the pool the
// agent degrades to console-only rescue on the machine side).
const rescueKernelArgs = "modules=loop,squashfs,sd-mod,usb-storage,ext4,vfat console=tty0 console=ttyS0,115200"

// rescueTaskContext is the rescue flow's context marker: the media facts
// (token + URI) share the install context's JSON keys on purpose — the
// shared cleanup paths (cleanupBootMedia, releaseBootPayloadFresh) parse
// them without knowing the flow.
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
// carrier idempotently (build overwrites, BMC re-arm is the same dance).
func (e *Executor) runRescueStage(ctx context.Context, task *store.Task, job *store.Job) error {
	a, err := decodeAction(job.Action)
	if err != nil {
		return err
	}
	if e.ProbeAlpineISO == "" {
		return classifiedErr("RESCUE_MEDIA_FAILED", false,
			"rescue needs the alpine carrier ISO (set MAMMOTH_PROBE_ALPINE_ISO) — it is the boot media the BMC mounts")
	}
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	// Re-entry check: a report persisted before this attempt (retry after a
	// lost ack) is the terminal fact — release and complete.
	if rec := parseRescueRecord(task); rec != nil && rec.Status == "ok" {
		rctx := parseRescueContext(task)
		e.releaseRescue(ctx, task, rctx, reasonCompleted)
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

	// ── carrier build (idempotent; dd's vMedia composition) ─────────────
	// The extended pool (sshd's apk source) stages under the task's netboot
	// path; its absence degrades to console-only rescue — an honest
	// warning, not a failure (the agent reports the same on the machine
	// side).
	poolStaged := false
	if e.WindowsApplyAlpineISO != "" {
		extISO, terr := builder.EnsureISO(ctx, e.WindowsApplyAlpineISO, e.MediaDir)
		if terr != nil {
			obs.FromContext(ctx).WarnContext(ctx, "rescue extended pool fetch failed — console-only rescue",
				"err", terr.Error())
		} else if xerr := builder.ExtractIsoApks(ctx, "", extISO, filepath.Join(e.BootTreeDir, rctx.Token, "apks")); xerr != nil {
			obs.FromContext(ctx).WarnContext(ctx, "rescue extended pool staging failed — console-only rescue",
				"err", xerr.Error())
		} else {
			poolStaged = true
		}
	}
	kernelArgs := rescueKernelArgs
	if poolStaged {
		poolURL := strings.TrimSuffix(e.ExternalURL, "/") + "/netboot/files/" + rctx.Token + "/apks"
		kernelArgs += " alpine_repo=" + poolURL
	}

	overlayName, overlay, oerr := builder.AgentOverlay()
	if oerr != nil {
		return classifiedErr("RESCUE_MEDIA_FAILED", true,
			"rescue agent overlay build failed: %s", oerr.Error())
	}
	external := strings.TrimSuffix(e.ExternalURL, "/")
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

	mediaFile := filepath.Join("boot", fmt.Sprintf("boot-%s.iso", rctx.Token))
	_ = os.MkdirAll(filepath.Dir(filepath.Join(e.MediaDir, mediaFile)), 0o755)
	alpineISO, terr := builder.EnsureISO(ctx, e.ProbeAlpineISO, e.MediaDir)
	if terr != nil {
		return classifiedErr("RESCUE_MEDIA_FAILED", true,
			"alpine carrier ISO fetch failed: %s", terr.Error())
	}
	if berr := buildBootISO(ctx, alpineISO, filepath.Join(e.MediaDir, mediaFile), kernelArgs, seed, e.MediaWorkDir, e.MediaDir); berr != nil {
		return classifiedErr("RESCUE_MEDIA_FAILED", true,
			"rescue media build failed: %s", berr.Error())
	}
	if e.MediaUploader != nil {
		if _, perr := e.MediaUploader.Push(ctx, filepath.Join(e.MediaDir, mediaFile), mediaFile); perr != nil {
			return classifiedErr("RESCUE_MEDIA_FAILED", true,
				"rescue media relay push failed: %s", perr.Error())
		}
	}
	mediaURI := mediaURIFor(e.MediaBaseURI, mediaFile)
	if mediaURI != rctx.MediaURI {
		if err := e.Jobs.PatchTaskContext(ctx, task.ID, task.StageIndex, map[string]any{
			"media_uri": mediaURI,
		}); err != nil {
			return err
		}
		rctx.MediaURI = mediaURI
	}

	// ── arm: the virtual-media mount dance (probe/install's sequence) ──
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
	e.Events.Append(ctx, "task", task.ID, "task.stage_changed", map[string]any{
		"stage": "rescue", "detail": "rescue media booted — waiting for the agent's rescue-ready report",
	})
	obs.FromContext(ctx).InfoContext(ctx, "rescue media booted, waiting for report",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)

	// ── wait for the rescue-ready report (the callback persists it under
	// the context's "rescue" key) ────────────────────────────────────────
	wait := e.ProbeWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			e.releaseRescue(context.WithoutCancel(ctx), task, rctx, "canceled")
			return ctx.Err()
		case <-deadline.C:
			// No report: the machine never reached the rescue system (or
			// the report path is broken). Probe's semantics: power off and
			// release — retry re-arms from scratch.
			cctx := context.WithoutCancel(ctx)
			e.releaseRescue(cctx, task, rctx, "timeout")
			if c2, a2, p2, ok2 := e.outOfBand(cctx, task); ok2 {
				_, _ = e.BMC.Do(cctx, a2, c2, p2, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
					return nil, d.SetPower(ctx, a2, c2, bmc.PowerOff)
				})
			}
			return classifiedErr("RESCUE_TIMEOUT", true,
				"no rescue-ready report within %s — machine powered off, carrier released", wait.String())
		case <-tick.C:
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
				e.releaseRescue(ctx, task, rctx, reasonCompleted)
				// Boot order back to disk (best-effort): the one-shot CDROM
				// override is consumed, but a machine re-armed through
				// firmware defaults must return to its installed system on
				// the operator's reboot.
				if _, derr := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
					return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootDisk, false)
				}); derr != nil {
					obs.FromContext(ctx).WarnContext(ctx, "rescue boot-order restore failed (continuing)", "err", derr.Error())
				}
				e.Events.Append(ctx, "task", task.ID, "task.boot_rescue", map[string]any{
					"detail":   rec.Detail,
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

// releaseRescue is the rescue payload's cleanup: eject (completed only —
// terminal paths let the one-shot override expire) + media file reclaim
// (the shared boot-<token>.iso sweep) + boot-tree pool reclaim. The
// machine's in-memory system is untouched — it keeps running from RAM.
func (e *Executor) releaseRescue(ctx context.Context, task *store.Task, rctx rescueTaskContext, reason string) {
	if rctx.Token == "" {
		return
	}
	if reason == reasonCompleted && rctx.MediaURI != "" {
		e.ejectBootMediaBestEffort(ctx, task, &installTaskContext{Token: rctx.Token, MediaURI: rctx.MediaURI})
	}
	e.cleanupBootMedia(ctx, task, reason)
	if e.BootTreeDir != "" && rctx.Token != "" {
		removeBootTree(e.BootTreeDir, rctx.Token)
	}
	obs.FromContext(ctx).InfoContext(ctx, "rescue payload released",
		obs.FieldTaskID, task.ID, "reason", reason)
}

// releaseRescuePayloadFresh re-reads the task before the release (the
// claim-time snapshot predates the context patch — same discipline as the
// install path's releaseBootPayloadFresh).
func (e *Executor) releaseRescuePayloadFresh(ctx context.Context, task *store.Task, reason string) {
	fresh := task
	if t, err := e.Jobs.GetTask(ctx, task.ID); err == nil {
		fresh = t
	}
	e.releaseRescue(ctx, fresh, parseRescueContext(fresh), reason)
}
