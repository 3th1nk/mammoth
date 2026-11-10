package netboot

// Address-service observability (docs/09-roadmap.md「装机网段地址服务可
// 观测」, design finalized 2026-10-30): the provisioning segment's DHCP
// health is the highest-frequency root cause of silent install failures —
// site-DHCP flaps, no site DHCP, pool exhaustion all look like an install
// stuck forever. This file watches the wire inside the engine's own DHCP
// responder (builtin pool / proxy shapes) and turns repeated DISCOVERs
// without an address follow-up into ROOT-CAUSE lines on the task's log —
// compressing the 90-minute-timeout diagnosis to the first minutes.
//
// Three signals, one state machine per MAC:
//
//	discover-repeat: ≥3 DISCOVERs within the window with no addressing
//	  REQUEST (option-50 / ciaddr / bound-ip) in between:
//	    pool mode   → "site DHCP answered nothing for the installer kernel"
//	    proxy mode  → "segment has NO address service" (the installer's
//	                  plain kernel DISCOVER gets silence from everywhere —
//	                  the PXE stage may still boot, install never proceeds)
//	  A subsequent addressing REQUEST (the client got an address somewhere)
//	  clears the state — that's the healthy contrast signal.
//
// installed-orphan: the MAC's task reached a terminal state (reported by
//	  the runner via NoteTerminal) and the MAC KEEPS discovering → "the
//	  freshly installed machine has no address service" — a free install
//	  quality follow-up on the succeeded task's log.
//
// The lines go through the same task-attribution chain the syslog sink
// uses (TaskFor → slog with obs.FieldTaskID → the TaskLogTee persists into
// task_logs). Netboot keeps NO database dependency: the caller wires a
// TaskFor callback (lease-reverse + IPRegistry chain) and a Logger.

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/3th1nk/mammoth/internal/obs"
)

// Observer watches per-MAC DHCP discovery patterns. One instance lives on
// the Server (wired in Start when a TaskFor callback is provided).
type Observer struct {
	mu      sync.Mutex
	macs    map[string]*macWatch
	taskFor func(mac string) string
	log     *slog.Logger
	now     func() time.Time
}

// Tuneables (exported for tests; the defaults cover the 4s/8s DHCP client
// retry cadence with margin).
const (
	// discoverThreshold is the DISCOVER count that fires the root-cause
	// line: 3 repeats inside the window = the client is retrying against
	// silence (its own backoff is 4s/8s/16s...).
	discoverThreshold = 3
	// discoverWindow bounds one repeat burst; a DISCOVER outside the window
	// resets the counter (a lone re-discover hours later is not starvation).
	discoverWindow = 3 * time.Minute
	// watchTTL is how long a silent MAC stays remembered — long enough to
	// cover the install's kernel-retry phase, short enough that the map
	// self-cleans (the segment's MAC population is small).
	watchTTL = 30 * time.Minute
	// repeatSuppression keeps the root-cause line from spamming the task
	// log on every further DISCOVER (one line per this period per MAC).
	repeatSuppression = 5 * time.Minute
)

type macWatch struct {
	discoverN   int
	firstAt     time.Time
	lastAt      time.Time
	addressed   bool // an addressing REQUEST cleared the state
	warnedAt    time.Time
	terminalSig string // task's terminal marker ("" until reported)
}

// NewObserver wires the watcher. taskFor resolves the MAC's active task id
// ("") when the MAC maps to no task); log receives the attributed lines.
// taskFor may be nil at construction and late-wired via SetTaskFor —
// attribution degrades to engine-log-only until then.
func NewObserver(taskFor func(mac string) string, log *slog.Logger) *Observer {
	if log == nil {
		log = slog.Default()
	}
	return &Observer{
		macs:    map[string]*macWatch{},
		taskFor: taskFor,
		log:     log,
		now:     time.Now,
	}
}

// SetTaskFor late-wires the MAC→task attribution chain. serve constructs
// the observer before its resolver exists (cmd/mammoth/serve.go wires it
// right after the boot-entry registry is built); calling the nil func used
// to SIGSEGV the netboot handler on the first real-segment DISCOVER burst
// (2026-11-10 .132). Nil taskFor stays safe: attribution falls back to the
// engine-log-only path in NoteMessage.
func (o *Observer) SetTaskFor(taskFor func(mac string) string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.taskFor = taskFor
}

// NoteMessage feeds one parsed client message into the state machine.
// addressed = the message carries an address claim (option 50 / ciaddr /
// bound ip — the client GOT an address somewhere). discover = it is a
// DISCOVER (the client is still looking).
func (o *Observer) NoteMessage(mac string, msgType byte, addressed bool) {
	if o == nil || mac == "" {
		return
	}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()

	w, ok := o.macs[mac]
	if !ok || now.Sub(w.lastAt) > watchTTL {
		w = &macWatch{}
		o.macs[mac] = w
	}
	w.lastAt = now

	if addressed {
		// The healthy contrast signal: the client holds (or was just
		// granted) an address — clear any silence state.
		w.addressed = true
		w.discoverN = 0
		return
	}
	if msgType != msgDiscover {
		return
	}

	// The installed-orphan signal first: the MAC's task is terminal and it
	// STILL discovers — the freshly installed system has no address
	// service. One line on the terminal task's log (suppressed after).
	if w.terminalSig != "" {
		if w.warnedAt.IsZero() || now.Sub(w.warnedAt) >= repeatSuppression {
			o.emit(mac, w.terminalSig,
				"installed machine keeps discovering with no address service — the freshly installed system has no DHCP on this segment (declared static networks in the install spec avoid this)")
			w.warnedAt = now
		}
		return
	}

	// The discover-repeat signal.
	if w.addressed || w.firstAt.IsZero() || now.Sub(w.firstAt) > discoverWindow {
		w.discoverN = 1
		w.firstAt = now
		w.addressed = false
		return
	}
	w.discoverN++
	if w.discoverN < discoverThreshold {
		return
	}
	if w.warnedAt.IsZero() || now.Sub(w.warnedAt) >= repeatSuppression {
		var taskID string
		if o.taskFor != nil { // nil until SetTaskFor late-wires the chain
			taskID = o.taskFor(mac)
		}
		o.emit(mac, taskID,
			"repeated DISCOVER with no address follow-up — no DHCP answered on this segment (proxy mode: the segment has NO address service; pool mode: the site side is silent)")
		w.warnedAt = now
	}
}

// NoteTerminal marks the MAC's install task as terminal: from now on, its
// DISCOVERs are the installed-orphan signal (attributed to that task's
// log — the free install quality follow-up).
func (o *Observer) NoteTerminal(mac, taskID string) {
	if o == nil || mac == "" || taskID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	w, ok := o.macs[mac]
	if !ok {
		w = &macWatch{}
		o.macs[mac] = w
	}
	if o.now != nil {
		w.lastAt = o.now()
	}
	w.terminalSig = taskID
	w.warnedAt = o.now().Add(-repeatSuppression) // the orphan line gets its own budget
}

// emit writes the root-cause line with the task attribution field — the
// TaskLogTee (wired at serve startup) persists task-scoped lines into
// task_logs, so the operator sees the cause at the exact surface they
// already watch.
func (o *Observer) emit(mac, taskID, cause string) {
	args := []any{"mac", mac, "cause", cause}
	if taskID != "" {
		args = append(args, obs.FieldTaskID, taskID)
		o.log.Warn("dhcp address-service root cause", args...)
		return
	}
	// No task attribution: the wire carries a MAC nobody registered —
	// still diagnostic, engine-log only.
	o.log.Warn("dhcp address-service observation (no task attribution)", args...)
}

// String is a debug view (tests).
func (o *Observer) state(mac string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	w, ok := o.macs[mac]
	if !ok {
		return "none"
	}
	return fmt.Sprintf("n=%d addressed=%v terminal=%q", w.discoverN, w.addressed, w.terminalSig)
}
