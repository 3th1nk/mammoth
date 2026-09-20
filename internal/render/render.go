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
	"strings"
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
	// InstallerAutoReboot declares that the installer reboots by itself when
	// it finishes (anaconda's reboot, subiquity's shutdown:reboot). d-i
	// stalls on its "Installation complete — remove the media" confirmation
	// dialog instead; for those installers the pipeline issues the reboot
	// itself after the deferred media release.
	InstallerAutoReboot bool `json:"installer_auto_reboot,omitempty"`
	// NetbootKernelArgs overrides KernelArgs when the task boots via PXE —
	// the network-install source (preseed/url, casper nfsroot) is shaped
	// differently from the offline CD mount. Empty means the ISO args apply
	// verbatim (the RHEL case: inst.repo=nfs: serves both carriers).
	NetbootKernelArgs string `json:"netboot_kernel_args,omitempty"`
}

// NetbootInputs carries the PXE-install source locations (non-nil when the
// submission pinned boot.strategy=pxe, docs/06-install-pipeline.md §3.3):
// drivers with a network-install source shape their args / seed against it.
type NetbootInputs struct {
	// PoolURL is the HTTP install-source pool: the distro ISO's shared,
	// content-addressed unpack (<ExternalURL>/netboot/store/<sha256>/iso).
	PoolURL string
	// NFSRootURL is the NFS install-source tree in casper's nfsroot form
	// (host:/path — MediaBaseURI + the shared pool-store path).
	NFSRootURL string
	// InstallSMBUNC/User/Password feed the windows wimboot carrier's
	// startnet: WinPE maps this deployment-provided SMB export and runs
	// setup from it (the SMB analog of the NFS media export — docs/compat/
	// distros.md §windows). Empty UNC = no export configured; the submission
	// gate rejects windows PXE before anything renders.
	InstallSMBUNC      string
	InstallSMBUser     string
	InstallSMBPassword string
	// PoolPublicKey is the binary OpenPGP keyring of the pool signing key —
	// the HTTP pool is a signed offline mirror, and the installer's apt needs
	// this key in its trustdb to pass apt-setup's mirror verification. Nil
	// when no pool key is configured (unsigned pool: apt setup will fail).
	PoolPublicKey []byte
	// StaticIP/StaticRouter/StaticMask describe the DHCP-pool reservation
	// made for this machine at arm time (DHCP-carrier installs, ubuntu PXE):
	// the installer gets them as a static ip= kernel argument because the
	// casper initramfs's boot-time DHCP proved racy on real hardware (udev
	// renames the NIC mid-ipconfig), and the reserved address is sticky —
	// the installed system renews the same lease. Empty on deployments
	// without the DHCP pool (drivers fall back to their DHCP-only shape).
	StaticIP     string
	StaticRouter string
	StaticMask   string
}

// NetbootCarrier declares where a distro's PXE boot files come from.
type NetbootCarrier string

const (
	// NetbootCarrierISO — the boot files extract from the distro ISO itself
	// (RHEL pxeboot, casper).
	NetbootCarrierISO NetbootCarrier = "iso"
	// NetbootCarrierDINetboot — the ISO's d-i initrd is the cdrom flavour,
	// useless over the wire; the boot files come from the distro's official
	// netboot tarball instead (a deployment-configured carrier,
	// MAMMOTH_PXE_DI_NETBOOT).
	NetbootCarrierDINetboot NetbootCarrier = "di_netboot"
	// NetbootCarrierAlpineNetboot — the agent installer's carrier: the alpine
	// NETBOOT tarball (MAMMOTH_PROBE_ALPINE_NETBOOT, shared with the ramdisk
	// probe) supplies kernel/initrd/modloop, and the distro ISO contributes
	// its /apks package repository. The agent's apkovl overlay rides the
	// boot tree and is fetched by URL (apkovl=).
	NetbootCarrierAlpineNetboot NetbootCarrier = "alpine_netboot"
	// NetbootCarrierWimboot — the Windows carrier: wimboot assembles the
	// WinPE memory environment from the media's own boot files, and the
	// augmented boot.wim carries the answer file AND the install source, so
	// there is no unpacked pool tree at all (NetbootPoolNone). Delivered by
	// iPXE — the only documented wimboot host (docs: ipxe.org/wimboot).
	NetbootCarrierWimboot NetbootCarrier = "wimboot"
)

// NetbootPool declares how a distro's PXE install source is served.
type NetbootPool string

const (
	// NetbootPoolNone — no unpacked source tree needed (RHEL: anaconda
	// mounts the NFS-hosted ISO directly via inst.repo).
	NetbootPoolNone NetbootPool = ""
	// NetbootPoolHTTP — the ISO unpacks under the boot tree and serves as a
	// plain HTTP pool (d-i's mirror: dists/ + pool/, offline semantics kept).
	NetbootPoolHTTP NetbootPool = "http_pool"
	// NetbootPoolNFS — the ISO unpacks under the boot tree and is consumed
	// over NFS (casper's netboot=nfs squashfs root).
	NetbootPoolNFS NetbootPool = "nfs_tree"
)

// NetbootInstallDriver is the optional capability describing how a distro
// boots and sources its installer over PXE (docs/06-install-pipeline.md
// §3.3). Drivers that do not implement it get the RHEL treatment: boot
// files from the ISO, no unpacked source tree.
type NetbootInstallDriver interface {
	NetbootCarrier() NetbootCarrier
	NetbootPool() NetbootPool
}

// NetbootInstallOf reports a driver's PXE carrier and install-source pool.
func NetbootInstallOf(d OSDriver) (NetbootCarrier, NetbootPool) {
	if n, ok := d.(NetbootInstallDriver); ok {
		return n.NetbootCarrier(), n.NetbootPool()
	}
	return NetbootCarrierISO, NetbootPoolNone
}

// AgentInstaller is the optional capability marking drivers whose install
// runtime is the mammoth agent initramfs (docs/12-agent-initramfs.md) — a
// distro installer never runs on the machine; the agent partitions the disks
// and installs the base system from the pool itself. The boot carrier must
// add the agent's apkovl overlay (built by the builder) to the boot media in
// addition to the rendered answer files.
type AgentInstaller interface {
	AgentInstaller() bool
}

// IsAgentInstaller reports whether the driver runs the agent install runtime.
func IsAgentInstaller(d OSDriver) bool {
	a, ok := d.(AgentInstaller)
	return ok && a.AgentInstaller()
}

// ── resolved inputs (produced by verify_layout / orchestration) ─────────────

// ResolvedDisk is a selector-resolved install target.
type ResolvedDisk struct {
	Device string `json:"device"` // kernel name, e.g. nvme0n1
	Serial string `json:"serial,omitempty"`
	// SizeBytes carries the inventory-reported capacity — %pre uses it to
	// re-identify the device when Device is not a kernel name (Redfish
	// logical drive names differ from installer device names).
	SizeBytes int64 `json:"size_bytes,omitempty"`
	Wipe      bool  `json:"wipe"`
	KeepDisk  bool  `json:"keep_disk,omitempty"` // keep: disk — untouched
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

// NormalizeESP auto-applies the esp flag to /boot/efi partitions that do
// not carry it. Curtin (autoinstall) and the agent runtime require the flag
// to type the partition as an EFI System Partition — unlike anaconda, which
// infers it from the /boot/efi mountpoint (real-hardware 2288H: a bare
// /boot/efi rendered a plain fat32 partition and subiquity rejected the
// whole storage config with "did not create needed bootloader partition").
// Mutates the slice elements in place; callers own the input view.
func NormalizeESP(disks []ResolvedDisk) {
	for i := range disks {
		for j, p := range disks[i].Partitions {
			if p.Mount == "/boot/efi" && !hasEspFlag(p.Flags) {
				disks[i].Partitions[j].Flags = append(append([]string(nil), p.Flags...), "esp")
			}
		}
	}
}

func hasEspFlag(flags []string) bool {
	for _, f := range flags {
		if f == "esp" {
			return true
		}
	}
	return false
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

	// Raid carries declarative RAID volumes (docs/09-roadmap.md M6):
	// software → kickstart raid lines; hardware → bound device discovered
	// by the configure_raid stage.
	Raid []ResolvedRaid `json:"raid,omitempty"`

	// DriftCheck enables the %pre layout drift guard (policy.verify_layout,
	// default true; docs/09-roadmap.md M4).
	DriftCheck bool

	// Netboot, non-nil when the submission pinned boot.strategy=pxe: the
	// driver shapes its netboot answer files and kernel args against the
	// network install source (docs/06-install-pipeline.md §3.3).
	Netboot *NetbootInputs
}

// ResolvedRaid is one RAID volume with resolved member devices.
type ResolvedRaid struct {
	Name    string   `json:"name"`
	Level   string   `json:"level"`   // 0 | 1 | 5 | 10
	Mode    string   `json:"mode"`    // software | hardware
	Members []string `json:"members"` // resolved member device names
	// MemberSerials parallels Members — the volume-creation capability
	// identifies drives by serial (controllers rename volumes; serials carry
	// the intent, docs/compat/huawei.md).
	MemberSerials []string `json:"member_serials,omitempty"`
	// BoundDevice: for hardware mode, the logical drive's discovered kernel
	// name (configure_raid binds it via re-inventory).
	BoundDevice string `json:"bound_device,omitempty"`
	// SizeBytes: the bound volume's capacity — %pre re-identifies non-kernel
	// bound names (controller-assigned LogicalDriveN) by size.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// VolumeSerial: the bound volume's SCSI serial from the refreshed
	// in-band snapshot — curtin (ubuntu) identifies drives by serial.
	VolumeSerial string              `json:"volume_serial,omitempty"`
	Partitions   []ResolvedPartition `json:"partitions,omitempty"`
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

// PXEDriver is the optional capability declaring a distro's network-boot
// (PXE/iPXE) support level for install submissions with boot.strategy=pxe
// (docs/06-install-pipeline.md §3.3, §6). Drivers that do not implement it
// are treated as SupportNone — third-party drivers keep compiling unchanged.
type PXEDriver interface {
	PXESupport() SupportLevel
}

// PXESupport reports a driver's network-boot support level.
func PXESupport(d OSDriver) SupportLevel {
	if p, ok := d.(PXEDriver); ok {
		return p.PXESupport()
	}
	return SupportNone
}

// FirmwareSupport declares which client firmware a distro's install media
// can boot (docs/08-data-model.md machines.pxe_firmware, docs/09-roadmap.md
// boot 策略门禁).
type FirmwareSupport string

const (
	// FirmwareAll — BIOS + UEFI. The default: most distro media carries
	// both boot paths.
	FirmwareAll FirmwareSupport = "all"
	// FirmwareUEFIOnly — the media dropped its BIOS boot images upstream
	// (rocky10): a BIOS-firmware machine can neither PXE nor virtual-media
	// boot it, so the mismatch must be caught at submission.
	FirmwareUEFIOnly FirmwareSupport = "uefi_only"
	// FirmwareBIOSOnly — reserved; no driver member today.
	FirmwareBIOSOnly FirmwareSupport = "bios_only"
)

// FirmwareDriver is the optional capability declaring the distro media's
// firmware range. Drivers that do not implement it are treated as
// FirmwareAll — third-party drivers keep compiling unchanged (same pattern
// as PXEDriver).
type FirmwareDriver interface {
	FirmwareSupport() FirmwareSupport
}

// FirmwareSupportOf reports a driver's media firmware range.
func FirmwareSupportOf(d OSDriver) FirmwareSupport {
	if f, ok := d.(FirmwareDriver); ok {
		return f.FirmwareSupport()
	}
	return FirmwareAll
}

// Allows reports whether a client firmware can boot this media. fw is the
// observed option-93 label ("bios", "ia32", "uefi-x64", "uefi-arm64");
// ia32 counts as legacy BIOS-class (no UEFI).
func (f FirmwareSupport) Allows(fw string) bool {
	uefi := strings.HasPrefix(fw, "uefi")
	switch f {
	case FirmwareUEFIOnly:
		return uefi
	case FirmwareBIOSOnly:
		return !uefi
	default:
		return true
	}
}

// FamilyDriver is the optional capability reporting the distro's installer
// family ("kickstart", "autoinstall", "preseed", "agent") — the
// support-matrix surface telling API consumers which install path a distro
// rides. Family membership is a driver-level fact (how the dialect drives
// the installer), not per-distro data.
type FamilyDriver interface {
	Family() string
}

// FamilyOf reports a driver's installer family ("" when undeclared —
// third-party drivers keep compiling unchanged).
func FamilyOf(d OSDriver) string {
	if f, ok := d.(FamilyDriver); ok {
		return f.Family()
	}
	return ""
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
