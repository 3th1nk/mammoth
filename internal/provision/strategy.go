package provision

import (
	"context"
	"encoding/json"

	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
)

// Boot carrier strategies (docs/06-install-pipeline.md §3): how the
// installer reaches the machine. virtual_media repacks the distro ISO into
// a per-task boot ISO the BMC mounts; pxe boots the installer over the LAN
// (proxyDHCP + iPXE, the netboot service serves the payload).
type bootStrategyName string

const (
	strategyVirtualMedia bootStrategyName = "virtual_media"
	strategyPXE          bootStrategyName = "pxe"
)

// bootSession bundles one prepare/arm/release round's inputs. Everything is
// explicit — no hidden reads of Executor state — so strategies unit-test
// against fake drivers and in-memory stores.
type bootSession struct {
	Task    *store.Task
	Job     *store.Job
	Spec    *installSpecView
	Ictx    *installTaskContext
	Answers []render.AnswerFile
	Boot    render.BootParams
	// Inputs carries the render inputs the answers came from — the
	// carrier/payload choice in prepare reads the installer declaration
	// from here (windows: setup → wimboot, agent → alpine netboot).
	Inputs render.InstallInputs
	// Seed carries extra boot-media seed files beyond the rendered answers
	// (the agent installer's apkovl overlay — binary content that must not
	// round-trip through the JSON task context).
	Seed map[string]string
}

// bootStrategy is the seam between "make the installer reachable" and the
// pipeline stages. prepare runs in prepare_media (payload + registry),
// arm in boot (point the firmware, power), release reclaims everything.
// Methods are stage-idempotent: retries re-enter them like any stage body.
type bootStrategy interface {
	name() bootStrategyName
	prepare(ctx context.Context, s *bootSession) error
	arm(ctx context.Context, s *bootSession) error
	release(ctx context.Context, s *bootSession, reason string)
}

// bootStrategyFor resolves the effective strategy: the spec's
// boot.strategy wins, else the deployment default. Unknown values are
// rejected here too — the submit-side gate is advisory against stale
// runners, this one is authoritative.
func (e *Executor) bootStrategyFor(spec *installSpecView) (bootStrategy, error) {
	name, known := effectiveStrategyName(e, spec)
	if !known {
		return nil, classifiedErr("SCHEMA_INVALID_BOOT_STRATEGY", false,
			"unknown boot.strategy %q (want virtual_media|pxe)", spec.Boot.Strategy)
	}
	if name == strategyPXE && e.Netboot == nil {
		return nil, classifiedErr("NETBOOT_UNAVAILABLE", false,
			"boot.strategy=pxe needs the netboot service (MAMMOTH_PXE_ENABLED) on this runner")
	}
	if name == strategyPXE {
		return &pxeStrategy{e: e}, nil
	}
	return &virtualMediaStrategy{e: e}, nil
}

// effectiveStrategyName resolves the strategy the same way bootStrategyFor
// does (spec declaration wins, else deployment default) — usable before the
// strategy object exists, e.g. at render time. Unknown declarations return
// known=false with the virtual_media fallback; bootStrategyFor turns that
// into the authoritative rejection, render-time callers just proceed (the
// task fails at bootStrategyFor before anything ships).
func effectiveStrategyName(e *Executor, spec *installSpecView) (bootStrategyName, bool) {
	if spec != nil && spec.Boot.Strategy != "" {
		switch spec.Boot.Strategy {
		case "pxe":
			return strategyPXE, true
		case "virtual_media":
			return strategyVirtualMedia, true
		default:
			return strategyVirtualMedia, false
		}
	}
	if e.BootStrategyDefault == "pxe" {
		return strategyPXE, true
	}
	return strategyVirtualMedia, true
}

// installPath names the windows install pathway: the setup.exe black box
// (the default, booted through the wimboot carrier) or the agent
// apply-image flow (wimlib apply + pre-baked BCD through the alpine agent
// carrier — docs/compat/distros.md §windows, 通路路线决策).
type installPath string

const (
	pathSetup installPath = "setup"
	pathAgent installPath = "agent"
)

// effectiveInstallPath resolves boot.installer: a non-empty spec declaration
// wins ("setup" | "agent"), else the driver default (setup — the agent path
// is explicit opt-in until it hardens on real hardware, docs/09-roadmap.md
// 影子决策 step). Unknown declarations return known=false; the submit gate
// turns that into SCHEMA_INVALID_BOOT_INSTALLER.
func effectiveInstallPath(spec *installSpecView) (installPath, bool) {
	if spec == nil || spec.Boot.Installer == "" {
		return pathSetup, true
	}
	switch spec.Boot.Installer {
	case "setup":
		return pathSetup, true
	case "agent":
		return pathAgent, true
	default:
		return pathSetup, false
	}
}

// releaseBootPayload reclaims whichever payload a task produced — the
// unified cleanup entry for every terminal path (completed / terminal
// failure / cancel / retry rebuild). In-flight tasks from before the
// strategy marker exist in no strategy field: they were virtual media by
// definition, so the empty case keeps the legacy behavior.
func (e *Executor) releaseBootPayload(ctx context.Context, task *store.Task, ictx *installTaskContext, reason string) {
	if ictx == nil || ictx.Token == "" {
		return
	}
	var s *bootSession
	switch bootStrategyName(ictx.BootStrategy) {
	case strategyPXE:
		s = &bootSession{Task: task, Ictx: ictx}
		e.pxe().release(ctx, s, reason)
	default:
		s = &bootSession{Task: task, Ictx: ictx}
		e.virtualMedia().release(ctx, s, reason)
	}
}

func (e *Executor) virtualMedia() *virtualMediaStrategy { return &virtualMediaStrategy{e: e} }
func (e *Executor) pxe() *pxeStrategy                   { return &pxeStrategy{e: e} }

// releaseReason distinguishes the completed path (eject BEFORE file reclaim,
// grace-period semantics) from terminal paths (files only — the BMC's
// one-shot override has expired by then and ejecting adds nothing).
const (
	reasonCompleted = "completed"
)

// releasePXE cleans one PXE task's footprint: registry rows + boot tree.
func (e *Executor) releasePXE(ctx context.Context, taskID, token, reason string) {
	if e.Netboot != nil {
		if deleted, err := e.Netboot.DeleteByTask(ctx, taskID); err == nil && deleted {
			e.Events.Append(ctx, "task", taskID, "task.netboot_released", map[string]any{
				"reason": reason,
			})
		}
	}
	if token != "" && e.BootTreeDir != "" {
		removeBootTree(e.BootTreeDir, token)
	}
	obs.FromContext(ctx).InfoContext(ctx, "netboot payload released",
		obs.FieldTaskID, taskID, "reason", reason)
}

// netbootRecord is the task-context marker for a PXE-armed machine.
type netbootRecord struct {
	Token string   `json:"token"` // boot tree directory name (== task token)
	MACs  []string `json:"macs"`  // registered NICs (diagnostics; release is by task)
}

// parseInstallContext decodes a task context tolerantly (cleanup paths must
// never fail on a corrupt context — they run inside error handlers).
func parseInstallContext(task *store.Task) *installTaskContext {
	var ictx installTaskContext
	if len(task.Context) == 0 || json.Unmarshal(task.Context, &ictx) != nil {
		return nil
	}
	return &ictx
}
