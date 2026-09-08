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
}

// VolumeCreator is the optional capability of building RAID volumes on the
// controller (docs/07-bmc.md §5: OEM/extended capabilities enter behind
// optional interfaces). Drivers without it simply don't implement the
// interface; the pipeline reports BMC_UNSUPPORTED.
type VolumeCreator interface {
	// CreateVolume builds a RAID volume from the storage controller's
	// drives. Idempotent by Name: an existing volume with the same name is
	// reported as "already exists" rather than rebuilt.
	CreateVolume(ctx context.Context, addr string, cred Credentials, spec VolumeSpec) error
}
