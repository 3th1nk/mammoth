// Package bmc normalizes heterogeneous out-of-band management into one
// resource model consumed by the install pipeline and the generic power /
// boot / media API. Protocol adapters: Redfish first, IPMI fallback, fake for
// tests (docs/07-bmc.md).
package bmc

// Protocol names a driver implementation.
type Protocol string

const (
	ProtocolRedfish Protocol = "redfish"
	ProtocolIPMI    Protocol = "ipmi"
	ProtocolFake    Protocol = "fake"
	ProtocolAuto    Protocol = "auto"
)

// PowerAction is a chassis power operation.
type PowerAction string

const (
	PowerOn    PowerAction = "power_on"
	PowerOff   PowerAction = "power_off" // hard power-off
	SoftOff    PowerAction = "soft_off"  // graceful shutdown
	HardReboot PowerAction = "hard_reboot"
	SoftReboot PowerAction = "soft_reboot"
	Cycle      PowerAction = "cycle" // hard off → on
)

// PowerState is the observed chassis power state.
type PowerState string

const (
	PowerStateOn      PowerState = "on"
	PowerStateOff     PowerState = "off"
	PowerStateUnknown PowerState = "unknown"
)

// BootDevice is a boot source override target.
type BootDevice string

const (
	BootPXE   BootDevice = "pxe"
	BootDisk  BootDevice = "disk"
	BootCDROM BootDevice = "cdrom"
	BootBIOS  BootDevice = "bios"
)

// Credentials are the out-of-band login. They originate from the encrypted
// credential store and never leave the BMC layer boundary other than to the BMC.
type Credentials struct {
	Username string
	Password string
}

// MediaImage references an image for virtual media mount. Remote-URI mounts
// and local-staging mounts are normalized behind MountMedia (docs/07-bmc.md §2).
type MediaImage struct {
	URL string
}

// BMCInfo is what Probe reports: identity and capability of the management
// controller. Vendor/model/firmware feed machine discovery backfill.
type BMCInfo struct {
	Protocol        Protocol
	Vendor          string
	Model           string
	FirmwareVersion string
	SerialNumber    string
	PowerState      PowerState
}

// HardwareView is the out-of-band hardware inventory shape (docs/04-install-spec.md §1,
// docs/05-inventory.md §2). Coverage annotations follow docs/05-inventory.md:
// known blind spots (vendor matrix hits, unenumerable RAID volumes, missing
// storage resources) mark the view partial instead of failing it.
type HardwareView struct {
	SerialNumber  string     `json:"serial_number,omitempty"`
	CPU           CPUView    `json:"cpu"`
	MemoryBytes   int64      `json:"memory_bytes,omitempty"`
	Disks         []DiskView `json:"disks,omitempty"`
	NICs          []NICView  `json:"nics,omitempty"`
	Coverage      Coverage   `json:"coverage"`
	CoverageNotes []string   `json:"coverage_notes,omitempty"`
}

// Coverage expresses completeness of a spec-level inventory (docs/05-inventory.md §2).
type Coverage string

const (
	CoverageFull    Coverage = "full"
	CoveragePartial Coverage = "partial"
)

// Note appends a partial-coverage reason and downgrades coverage.
func (h *HardwareView) Note(reason string) {
	h.Coverage = CoveragePartial
	h.CoverageNotes = append(h.CoverageNotes, reason)
}

type CPUView struct {
	Model string `json:"model,omitempty"`
	Cores int    `json:"cores,omitempty"`
}

type DiskView struct {
	Name      string `json:"name"`
	Serial    string `json:"serial,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Medium    string `json:"medium,omitempty"` // ssd | hdd | unknown
	Protocol  string `json:"protocol,omitempty"`
	Removable bool   `json:"removable,omitempty"`
}

type NICView struct {
	Name       string `json:"name"`
	MAC        string `json:"mac,omitempty"`
	SpeedMbps  int    `json:"speed_mbps,omitempty"`
	LinkUp     bool   `json:"link_up,omitempty"`
	PCIAddress string `json:"pci_address,omitempty"`
}
