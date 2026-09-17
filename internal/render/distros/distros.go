// Package distros is the declarative distro registry (docs/06-install-pipeline.md
// §5, the Cobbler distro_signatures shape): every distro mammoth can install
// is one entry in distros.json — family, ISO layout family, package-pool
// capability, arch set, firmware range, and the family's installer-generation
// parameters. Adding a distro of a KNOWN family is a JSON entry, zero Go;
// what stays Go is the dialect template logic itself (kickstart/preseed/
// autoinstall/agent templates encode how an installer is driven, which is
// not per-distro data).
//
// Two kinds of knowledge are deliberately NOT in this file: keep/pxe
// support and the netboot carrier/pool are properties of the FAMILY (how
// the dialect drives the installer over the network), exported by the
// drivers themselves into the support matrix; the ISO layout heuristics
// (DetectLayout) stay builder-internal — this file declares which layout
// family a distro's ISO belongs to for documentation and validation, the
// builder still probes the actual image.
package distros

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed distros.json
var raw []byte

// Known layout families (builder DetectLayout). Declared per distro as
// validated documentation — the matrix answer to "which bootloader shape
// does this ISO carry".
const (
	LayoutRHEL     = "rhel"
	LayoutRHEL10   = "rhel10"
	LayoutCasper   = "casper"
	LayoutDebianDI = "debian-di"
	LayoutAlpine   = "alpine"
)

// Pool capability of the distro's bundled package repository (docs/12
// §6): system_pool means the ISO's own pool can install a complete system;
// boot_pool means it only bootstraps the installer itself (the alpine
// STANDARD ISO trap — its /apks carries no kernel, installs boot fine and
// a system never). Advisory metadata today: consumed by docs and future
// image validation, not enforced at render (the pool is a property of the
// concrete image, which mammoth verifies only by unpacking).
const (
	PoolSystem = "system_pool"
	PoolBoot   = "boot_pool"
)

// KickstartProfile carries the installer-generation deltas inside the
// kickstart family (the former dialect() switch). One wrong flag here is a
// real-machine lesson — see docs/compat/distros.md for the incidents.
type KickstartProfile struct {
	// HostnameViaNetworkCmd: carry the hostname via `network --hostname=`.
	// CentOS 7's anaconda predates the parser guarantees and rocky9 ignores
	// the command on real hardware — those carry it via %post instead.
	HostnameViaNetworkCmd bool `json:"hostname_via_network_cmd"`
	// RootExtension: the %post sfdisk root-extension safety net can work
	// (util-linux 2.23 on CentOS 7 cannot resize GPT — the script must not
	// be rendered at all).
	RootExtension bool `json:"root_extension"`
	// NetRepair: %pre repairs kylin V10's mangled ifcfg writer (missing '=',
	// malformed UUID) and re-manages the NIC through NM, or the text install
	// blocks at the network spoke forever (V10 SP3 2403, real machine).
	NetRepair bool `json:"net_repair"`
	// DeviceByMAC: `network --device=` takes the MAC literal (paired with
	// NetRepair; the mangled writer path is the resolved-name form).
	DeviceByMAC bool `json:"device_by_mac"`
	// Extras are raw kickstart lines appended to the answer (uniontechos:
	// its Finish task group expects an EULA ack and a created user, else it
	// crashes with "max() arg is an empty sequence").
	Extras string `json:"extras,omitempty"`
}

// PreseedProfile is the preseed family's per-distro data: the archive
// codename the HTTP pool's dists/ carries.
type PreseedProfile struct {
	Suite string `json:"suite"`
}

// AgentProfile is the agent family's per-distro data (docs/12): what the
// agent installs from the pool and which packages provide the bootloader
// per firmware. The runtime script is alpine-shaped; a different agent OS
// would be a new family, not a new entry.
type AgentProfile struct {
	// Packages is the target base system installed from the pool.
	Packages []string `json:"packages"`
	// BootloaderBIOS / BootloaderUEFI are the bootloader package sets per
	// firmware, installed into the target before grub-install.
	BootloaderBIOS []string `json:"bootloader_bios"`
	BootloaderUEFI []string `json:"bootloader_uefi"`
	// Tools is the agent's own toolchain installed into the memory root
	// (partitioning, mkfs, hashing).
	Tools []string `json:"tools"`
}

// Declaration is one distro entry.
type Declaration struct {
	Name   string `json:"name"`
	Family string `json:"family"` // kickstart | autoinstall | preseed | agent
	// Layout is the ISO's boot-layout family (Layout* constants).
	Layout string `json:"layout"`
	// Pool is the bundled package pool capability (Pool* constants).
	Pool string `json:"pool,omitempty"`
	// Archs lists the installable architectures.
	Archs []string `json:"archs"`
	// Firmware is the media's bootable firmware range
	// (all | uefi_only | bios_only) — the FirmwareDriver value.
	Firmware string `json:"firmware"`

	Kickstart *KickstartProfile `json:"kickstart,omitempty"`
	Preseed   *PreseedProfile   `json:"preseed,omitempty"`
	Agent     *AgentProfile     `json:"agent,omitempty"`
}

type file struct {
	Version int           `json:"version"`
	Distros []Declaration `json:"distros"`
}

var loaded file

func init() {
	if err := json.Unmarshal(raw, &loaded); err != nil {
		panic(fmt.Sprintf("distros: embedded distros.json is invalid: %v", err))
	}
	if err := validate(loaded); err != nil {
		panic(fmt.Sprintf("distros: embedded distros.json rejected: %v", err))
	}
}

// validate enforces the invariants the families rely on: unique names,
// known families/layouts/pools/firmware values, non-empty archs, and the
// family-specific payload present (and suite non-empty for preseed,
// package sets non-empty for agent).
func validate(f file) error {
	if f.Version != 1 {
		return fmt.Errorf("unsupported version %d", f.Version)
	}
	knownFamilies := map[string]bool{"kickstart": true, "autoinstall": true, "preseed": true, "agent": true}
	knownLayouts := map[string]bool{LayoutRHEL: true, LayoutRHEL10: true, LayoutCasper: true, LayoutDebianDI: true, LayoutAlpine: true}
	knownPools := map[string]bool{"": true, PoolSystem: true, PoolBoot: true}
	knownFirmware := map[string]bool{"all": true, "uefi_only": true, "bios_only": true}
	seen := map[string]bool{}
	for _, d := range f.Distros {
		if d.Name == "" {
			return fmt.Errorf("entry with empty name")
		}
		if seen[d.Name] {
			return fmt.Errorf("duplicate distro %q", d.Name)
		}
		seen[d.Name] = true
		if !knownFamilies[d.Family] {
			return fmt.Errorf("%s: unknown family %q", d.Name, d.Family)
		}
		if !knownLayouts[d.Layout] {
			return fmt.Errorf("%s: unknown layout %q", d.Name, d.Layout)
		}
		if !knownPools[d.Pool] {
			return fmt.Errorf("%s: unknown pool %q", d.Name, d.Pool)
		}
		if !knownFirmware[d.Firmware] {
			return fmt.Errorf("%s: unknown firmware %q", d.Name, d.Firmware)
		}
		if len(d.Archs) == 0 {
			return fmt.Errorf("%s: archs must not be empty", d.Name)
		}
		switch d.Family {
		case "preseed":
			if d.Preseed == nil || d.Preseed.Suite == "" {
				return fmt.Errorf("%s: preseed family needs a suite", d.Name)
			}
		case "agent":
			if d.Agent == nil || len(d.Agent.Packages) == 0 ||
				len(d.Agent.BootloaderBIOS) == 0 || len(d.Agent.BootloaderUEFI) == 0 ||
				len(d.Agent.Tools) == 0 {
				return fmt.Errorf("%s: agent family needs packages, bootloader sets and tools", d.Name)
			}
		}
	}
	return nil
}

// Declarations returns the validated entries, ordered by name.
func Declarations() []Declaration {
	out := append([]Declaration(nil), loaded.Distros...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names lists the declared distro names.
func Names() []string {
	var out []string
	for _, d := range loaded.Distros {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

// lookup finds one entry.
func lookup(distro string) (Declaration, bool) {
	for _, d := range loaded.Distros {
		if d.Name == distro {
			return d, true
		}
	}
	return Declaration{}, false
}

// KickstartFor resolves the kickstart profile. Unknown distros get the
// current-generation default (standard everything) — the same fallback the
// former dialect() switch had, so zero-value constructors keep working in
// tests.
func KickstartFor(distro string) KickstartProfile {
	if d, ok := lookup(distro); ok && d.Kickstart != nil {
		return *d.Kickstart
	}
	return KickstartProfile{HostnameViaNetworkCmd: true, RootExtension: true}
}

// FirmwareFor resolves the media firmware range ("" and unknown → "all").
func FirmwareFor(distro string) string {
	if d, ok := lookup(distro); ok {
		return d.Firmware
	}
	return "all"
}

// ArchsFor resolves the declared arch set (unknown → amd64-only).
func ArchsFor(distro string) []string {
	if d, ok := lookup(distro); ok && len(d.Archs) > 0 {
		return append([]string(nil), d.Archs...)
	}
	return []string{"amd64"}
}

// PreseedFor resolves the preseed profile (unknown → empty suite; the
// driver rejects suite-less PXE renders, same as the former switch).
func PreseedFor(distro string) PreseedProfile {
	if d, ok := lookup(distro); ok && d.Preseed != nil {
		return *d.Preseed
	}
	return PreseedProfile{}
}

// AgentFor resolves the agent profile (unknown → zero value; the driver
// rejects package-less renders).
func AgentFor(distro string) AgentProfile {
	if d, ok := lookup(distro); ok && d.Agent != nil {
		return *d.Agent
	}
	return AgentProfile{}
}
