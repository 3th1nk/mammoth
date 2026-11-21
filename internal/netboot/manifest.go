// ManifestGate is the edge-pool lease gate: an in-memory snapshot of the
// engine's armed/enroll MAC manifest, hot-reloaded from
// /netboot/pool-manifest.json by the edge's sync ticker. Decision stays on
// the engine, execution rides the edge — the gate answers exactly what the
// engine's own gate would (armed entries admit a lease; enroll-eligible
// MACs admit the zero-registration probe; anything else stays silent).
//
// Two responsibilities stay deliberately separated, mirroring the engine:
// armed entries ride Entry (lease gate + TFTP grub.cfg rendering + the
// wimboot NBP switch, all keyed off the full entry the manifest carries),
// while enroll MACs only ride EnrollEligible — an enroll probe has no
// per-MAC artifacts to render.
package netboot

import (
	"context"
	"encoding/json"
	"sync"
)

// ManifestArmed is one armed entry as the engine's manifest carries it —
// the full boot facts, not just the MAC: the TFTP renderer and the wimboot
// NBP switch read the same fields the engine's store rows hold.
type ManifestArmed struct {
	MAC        string `json:"mac"`
	Kind       string `json:"kind"`
	TaskID     string `json:"task_id,omitempty"`
	Kernel     string `json:"kernel,omitempty"`
	Initrd     string `json:"initrd,omitempty"`
	KernelArgs string `json:"kernel_args,omitempty"`
	Token      string `json:"token,omitempty"`
}

// ManifestEnroll is one enroll-eligible sighting (pending + unregistered).
type ManifestEnroll struct {
	MAC string `json:"mac"`
}

type manifestSnapshot struct {
	armed  map[string]Entry
	enroll map[string]struct{}
}

// ManifestGate is nil-safe on every method: a gate that never loaded (the
// pool configured but the manifest not synced yet) admits nobody — a dead
// pool, never an open one.
type ManifestGate struct {
	mu sync.RWMutex
	s  manifestSnapshot
}

// NewManifestGate returns an empty gate.
func NewManifestGate() *ManifestGate {
	return &ManifestGate{s: manifestSnapshot{
		armed:  map[string]Entry{},
		enroll: map[string]struct{}{},
	}}
}

// Load replaces the whole snapshot from the engine's manifest JSON. MACs
// are normalized to the lowercase colon form every other lookup uses.
// A parse error leaves the previous snapshot in place — a broken download
// must not open or close the gate by half.
func (g *ManifestGate) Load(data []byte) error {
	var m struct {
		Armed  []ManifestArmed  `json:"armed"`
		Enroll []ManifestEnroll `json:"enroll"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	s := manifestSnapshot{armed: map[string]Entry{}, enroll: map[string]struct{}{}}
	for _, a := range m.Armed {
		mac := normalizeMAC(a.MAC)
		if mac == "" {
			continue
		}
		s.armed[mac] = Entry{
			MAC: mac, Kind: a.Kind, TaskID: a.TaskID,
			Kernel: a.Kernel, Initrd: a.Initrd, KernelArgs: a.KernelArgs, Token: a.Token,
		}
	}
	for _, e := range m.Enroll {
		if mac := normalizeMAC(e.MAC); mac != "" {
			s.enroll[mac] = struct{}{}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.s = s
	return nil
}

// Entry implements Resolver: the armed snapshot only. An unknown MAC is
// (nil, nil) — the engine-side contract the lease gate double-checks.
func (g *ManifestGate) Entry(_ context.Context, mac string) (*Entry, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if e, ok := g.s.armed[normalizeMAC(mac)]; ok {
		e := e
		return &e, nil
	}
	return nil, nil
}

// Sizes reports the loaded snapshot's armed/enroll counts — the sync
// observability surface (a silent gate is indistinguishable from a dead
// sync loop).
func (g *ManifestGate) Sizes() (armed, enroll int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.s.armed), len(g.s.enroll)
}

// EnrollEligible implements the second lease-gate check: sighted +
// unregistered MACs from the manifest.
func (g *ManifestGate) EnrollEligible(mac string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.s.enroll[normalizeMAC(mac)]
	return ok
}
