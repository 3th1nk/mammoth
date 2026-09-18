package bmc

import "context"

// Driver is the unified out-of-band capability surface (docs/07-bmc.md §1).
// Every method must be safe to call concurrently for different addresses; the
// Registry serializes calls per address because BMCs are weak hardware and
// parallel requests have been observed to wedge firmware.
type Driver interface {
	// Name identifies the implementation ("redfish", "ipmi", "fake").
	Name() Protocol

	// Probe returns controller identity and capability, and verifies that the
	// address answers this protocol with the given credentials.
	Probe(ctx context.Context, addr string, cred Credentials) (BMCInfo, error)

	// PowerState observes the chassis power state.
	PowerState(ctx context.Context, addr string, cred Credentials) (PowerState, error)

	// SetPower performs a chassis power action.
	SetPower(ctx context.Context, addr string, cred Credentials, action PowerAction) error

	// SetBootDevice sets the boot source override; once limits it to the next
	// boot. When the BMC lacks one-shot support, the driver must return
	// BMC_UNSUPPORTED so the caller can plan compensations (restore boot order).
	SetBootDevice(ctx context.Context, addr string, cred Credentials, dev BootDevice, once bool) error

	// MountMedia attaches an image to the virtual media (slot selection and
	// remote-URI vs staged upload are driver-internal concerns).
	MountMedia(ctx context.Context, addr string, cred Credentials, img MediaImage) error

	// EjectMedia detaches the given image (slot-matching is driver-internal).
	EjectMedia(ctx context.Context, addr string, cred Credentials, img MediaImage) error

	// ConsoleURL returns a one-time virtual console URL. OEM-specific;
	// drivers return BMC_UNSUPPORTED where no known OEM mapping exists.
	ConsoleURL(ctx context.Context, addr string, cred Credentials) (string, error)

	// CollectInventory returns the hardware view. Full support lands with the
	// Redfish inventory probe (M1); IPMI yields the FRU-limited subset.
	CollectInventory(ctx context.Context, addr string, cred Credentials) (HardwareView, error)
}

// VolumeSpec declares a RAID volume on a storage controller (docs/09-roadmap.md
// M6 硬 RAID): Redfish Volume creation, or the fake equivalent.
type VolumeSpec struct {
	Name     string // volume label; the logical drive surfaces under this name
	RAIDType string // "RAID0" | "RAID1" | "RAID5" | "RAID10"
	// MemberSerials identifies the member physical drives by serial —
	// controllers name volumes themselves (Huawei iBMC assigns
	// LogicalDriveN), so drives, not the label, carry the intent.
	MemberSerials []string
}

// VolumeCreator is the optional capability of building RAID volumes on the
// controller (docs/07-bmc.md §5: OEM/extended capabilities enter behind
// optional interfaces). Drivers without it simply don't implement the
// interface; the pipeline reports BMC_UNSUPPORTED.
type VolumeCreator interface {
	// CreateVolume builds a RAID volume over the requested member drives.
	// Idempotent by members: an existing volume spanning exactly the
	// requested serials at the same level is returned as-is rather than
	// rebuilt. Returns the name the volume carries in inventory (the
	// controller may rename — Huawei assigns LogicalDriveN).
	CreateVolume(ctx context.Context, addr string, cred Credentials, spec VolumeSpec) (string, error)
}

// PhysicalDriveEnumerator is the optional capability of listing the
// controller's physical drives — the RAID member pool. Unlike Disks in
// HardwareView (which follow the volume-first presentation rule: the OS
// sees logical drives when the controller reports them), RAID intent
// always selects physical drives (docs/05-inventory.md §2).
type PhysicalDriveEnumerator interface {
	PhysicalDrives(ctx context.Context, addr string, cred Credentials) ([]DiskView, error)
}

// FirmwareComponent is one firmware inventory entry (docs/07-bmc.md §6):
// a controller-side firmware image and its version. Id carries the
// vendor's own identity (Redfish Id — stable across reads); Name and
// Version are informational and may be empty on vendors that report
// presence without a version.
type FirmwareComponent struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// FirmwareInventoryProvider is the optional read-only capability of listing
// the controller's firmware inventory (Redfish SoftwareInventory under the
// UpdateService; the first of the v1.1 BMC capability interfaces — pure
// read, the warm-up before any write-capable interface). Drivers without it
// simply don't implement the interface; discovery leaves the machine's
// firmware view empty rather than failing.
type FirmwareInventoryProvider interface {
	// FirmwareInventory lists the firmware components the controller
	// reports. Best-effort data: implementations should prefer a partial
	// list over an error when some entries fail to decode.
	FirmwareInventory(ctx context.Context, addr string, cred Credentials) ([]FirmwareComponent, error)
}

// SanitizeResult reports what one drive's secure erase actually did, for
// the NIST 800-88 sanitization record (docs/07-bmc.md §6.2): which purge
// mechanism the controller applied and when it finished.
type SanitizeResult struct {
	Serial    string `json:"serial"`
	Method    string `json:"method,omitempty"` // vendor-reported (block erase / crypto erase / overwrite); informational
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// DriveEraser is the optional capability of issuing the controller's
// secure erase on physical drives — the BMC-side half of NIST 800-88 media
// sanitization (docs/07-bmc.md §6.2). This is the most destructive
// capability in the surface: erased data is unrecoverable by design. The
// pipeline layers the two-stage confirmation and live-drive validation
// around it; the driver only speaks the protocol.
type DriveEraser interface {
	// SecureErase erases the physical drives identified by serial — the
	// same identity PhysicalDriveEnumerator reports. Implementations must
	// resolve every serial BEFORE touching any drive: one unknown serial
	// aborts the whole request rather than erasing a subset.
	SecureErase(ctx context.Context, addr string, cred Credentials, serials []string) ([]SanitizeResult, error)
}

// BiosSetter is the optional capability of reading and changing the
// server's BIOS configuration through the Redfish Bios resource (attribute
// table, vendor-neutral keys — docs/07-bmc.md §6). This is a HIGH-RISK
// capability: wrong attribute values can brick boot. The pipeline layers
// protections around it (two-stage confirmation, live-table validation);
// the driver itself only speaks the protocol.
type BiosSetter interface {
	// BiosAttributes returns the current attribute table. Values are the
	// vendor's own JSON types (bool/string/number) keyed by the vendor's
	// attribute names.
	BiosAttributes(ctx context.Context, addr string, cred Credentials) (map[string]any, error)
	// SetBiosAttributes writes the given attributes as PENDING values —
	// Redfish Bios semantics apply them at the next boot, not live. Keys
	// must exist in the vendor's attribute table; unknown or read-only
	// attributes are a protocol-level rejection.
	SetBiosAttributes(ctx context.Context, addr string, cred Credentials, attrs map[string]any) error
}
