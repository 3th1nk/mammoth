// Package render is the answer-file rendering layer (docs/06-install-pipeline.md §2.2, §5):
// rendering is a pure function render(spec, machine) → answer files. No
// external calls (no snapshot lookups, no BMC) — testable and deterministic.
// Templates are organized by distro dialect, contributed by distro drivers;
// adding a distro means adding a driver implementation plus templates with
// zero orchestration changes.
package render

import (
	"fmt"
	"sort"
	"sync"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// Arch is an installation architecture.
type Arch string

const (
	ArchAMD64 Arch = "amd64"
	ArchARM64 Arch = "arm64"
)

// SupportLevel declares how completely a driver supports keep-partition
// semantics; it feeds the distro support matrix and submit-time validation
// (docs/06-install-pipeline.md §5).
type SupportLevel string

const (
	SupportFull    SupportLevel = "full"
	SupportPartial SupportLevel = "partial"
	SupportNone    SupportLevel = "none"
)

// AnswerFile is one rendered file delivered via the task-token URL.
type AnswerFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// BootParams are the installer kernel arguments baked into the boot media.
type BootParams struct {
	KernelArgs string `json:"kernel_args"` // inst.ks=… inst.repo=…
	AnswerURL  string `json:"answer_url"`
}

// ── resolved inputs (produced by verify_layout / orchestration) ─────────────

// ResolvedDisk is a selector-resolved install target.
type ResolvedDisk struct {
	Device   string `json:"device"` // kernel name, e.g. nvme0n1
	Serial   string `json:"serial,omitempty"`
	Wipe     bool   `json:"wipe"`
	KeepDisk bool   `json:"keep_disk,omitempty"` // keep: disk — untouched
	// KeepParts (keep: partitions): existing partitions to remove (all
	// snapshot partitions not preserved); freed space hosts new partitions.
	Remove []string `json:"remove,omitempty"`
	// Baseline carries the bound snapshot facts for the %pre drift guard.
	Baseline   []BaselinePartition `json:"baseline,omitempty"`
	Partitions []ResolvedPartition `json:"partitions,omitempty"`
}

// BaselinePartition is one snapshot partition bound at verify_layout; the
// %pre guard compares the live table against exactly these numbers
// (docs/06-install-pipeline.md §4).
type BaselinePartition struct {
	Device     string `json:"device"` // full name, e.g. sda1
	Number     int    `json:"number"`
	StartBytes int64  `json:"start_bytes"`
	EndBytes   int64  `json:"end_bytes"`
	SizeBytes  int64  `json:"size_bytes"`
	UUID       string `json:"uuid,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
}

// ResolvedPartition is one declared partition on a resolved disk.
type ResolvedPartition struct {
	Mount  string   `json:"mount"`
	FS     string   `json:"fs"`
	SizeMB int      `json:"size_mb,omitempty"` // 0 + Grow for "rest"
	Grow   bool     `json:"grow"`
	Flags  []string `json:"flags,omitempty"` // e.g. esp
	// Preserve: reuse the existing partition unformatted, mounted by its
	// original UUID (docs/04-install-spec.md §5.1).
	Preserve bool   `json:"preserve,omitempty"`
	Number   int    `json:"number,omitempty"`
	UUID     string `json:"uuid,omitempty"`
	OnPart   string `json:"on_part,omitempty"` // full kernel name, e.g. sda1
}

// NetworkEntry is one network declaration (docs/04-install-spec.md §5.2).
type NetworkEntry struct {
	Match   *NetMatch `json:"match,omitempty"`
	SetName string    `json:"set_name,omitempty"`
	Bond    *NetBond  `json:"bond,omitempty"`
	VLAN    *NetVLAN  `json:"vlan,omitempty"`

	Addresses   []string   `json:"addresses,omitempty"` // CIDR
	Routes      []NetRoute `json:"routes,omitempty"`
	Nameservers []string   `json:"nameservers,omitempty"`
	Search      []string   `json:"search,omitempty"`
	MTU         int        `json:"mtu,omitempty"`
}

type NetMatch struct {
	MAC        string `json:"mac,omitempty"`
	PCIAddress string `json:"pci_address,omitempty"`
	Name       string `json:"name,omitempty"`
}

type NetBond struct {
	// Slaves reference interfaces by MAC — resolved to names at install time.
	SlavesMACs []string          `json:"slaves_macs,omitempty"`
	Mode       string            `json:"mode,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
}

type NetVLAN struct {
	ID   int    `json:"id,omitempty"`
	Link string `json:"link,omitempty"` // resolved parent (bond0 or iface name)
}

type NetRoute struct {
	To  string `json:"to"`
	Via string `json:"via"`
}

// ScriptEntry is a user script hook (docs/04-install-spec.md §5 scripts).
type ScriptEntry struct {
	Stage        string // pre_install | post_install
	Inline       string // decoded from content_base64
	URL          string
	ExpectedExit []int
}

// InstallInputs is everything a driver renders from. Inputs arrive resolved:
// storage selectors are concrete devices, the hostname is expanded, and the
// image source is dereferenced.
type InstallInputs struct {
	TaskToken     string
	MachineID     string
	Hostname      string
	ImageSource   string // installer image URL (distro ISO)
	RootPassword  string // per-task random when spec asked for generate
	SSHPublicKeys []string

	BootDrive string         // resolved bootloader device
	Disks     []ResolvedDisk `json:"disks"`
	Network   []NetworkEntry `json:"network,omitempty"`
	Scripts   []ScriptEntry  `json:"scripts,omitempty"`

	// AnswerBaseURL is the task-token URL base; the driver composes its own
	// answer file names on it (rocky9: /ks.cfg; ubuntu22: /user-data).
	AnswerBaseURL string
	CompleteURL   string // %post callback

	// DriftCheck enables the %pre layout drift guard (policy.verify_layout,
	// default true; docs/09-roadmap.md M4).
	DriftCheck bool
}

// MachineView is the machine context a renderer may consult (hardware facts
// only — the resolution of intent into concrete devices happens upstream).
type MachineView struct {
	ID       string
	Hostname string
	Hardware *bmc.HardwareView
}

// OSDriver is the distro dialect (docs/06-install-pipeline.md §5).
type OSDriver interface {
	Distro() string
	SupportedArchs() []Arch
	RenderAnswers(in InstallInputs, m MachineView) ([]AnswerFile, BootParams, error)
	KeepPartitionSupport() SupportLevel
}

// Registry routes specs to drivers by distro.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]OSDriver
}

func NewRegistry() *Registry { return &Registry{drivers: map[string]OSDriver{}} }

func (r *Registry) Register(d OSDriver) error {
	if d.Distro() == "" {
		return fmt.Errorf("render: driver without distro")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[d.Distro()] = d
	return nil
}

// For returns the driver for a distro ("rocky9", "ubuntu22", …).
func (r *Registry) For(distro string) (OSDriver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[distro]
	if !ok {
		var known []string
		for k := range r.drivers {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("render: no driver for distro %q (known: %v)", distro, known)
	}
	return d, nil
}

// Distros lists registered distros (support matrix surface).
func (r *Registry) Distros() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.drivers))
	for k := range r.drivers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
