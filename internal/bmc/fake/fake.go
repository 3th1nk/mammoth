// Package fake provides an in-process BMC simulator implementing the full
// bmc.Driver surface. It backs unit tests and development end-to-end runs
// (the M0 acceptance flow) without any hardware.
package fake

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// BMC is one simulated controller. All exported knobs are plain fields so
// tests can script failure modes deterministically.
type BMC struct {
	mu sync.Mutex

	Vendor     string
	Model      string
	Firmware   string
	Serial     string
	Power      bmc.PowerState
	BootDevice bmc.BootDevice
	BootOnce   bool
	Media      []bmc.MediaImage
	Hardware   *bmc.HardwareView

	Volumes []bmc.VolumeSpec

	// FirmwareList scripts the firmware inventory the controller reports
	// (docs/07-bmc.md §6); defaultFirmware fills a plausible pair.
	FirmwareList []bmc.FirmwareComponent

	// SensorList scripts the health snapshot the controller reports
	// (docs/07-bmc.md §6.3); defaultSensors fills a plausible set.
	SensorList []bmc.SensorReading
	// PowerScripted overrides the chassis power state Health reports;
	// empty falls through to the Power field.
	PowerScripted bmc.PowerState

	// SELList scripts the system event log the controller reports
	// (docs/07-bmc.md §6.3); defaultSEL fills a plausible pair.
	SELList []bmc.SELEntry

	// BIOS is the simulated BIOS attribute table (docs/07-bmc.md §6);
	// defaultBIOS fills a plausible set. Writes apply immediately and
	// reject unknown attribute names (the protocol-fidelity the two-stage
	// validation tests against).
	BIOS map[string]any

	// ErasedSerials records what SecureErase has destroyed on this
	// controller (docs/07-bmc.md §6.2); unknown serials are rejected with
	// the same "resolve before touching" semantics the redfish driver
	// implements. EraseSupport defaults to true (secure-erase actions are
	// the norm on controllers that expose drive resources); script it off
	// to simulate a controller whose drives cannot be purged.
	ErasedSerials []string
	EraseSupport  bool

	// Failures scripts error injection: set before the call under test.
	FailOps map[string]error

	// ConsoleBaseURL is the prefix for generated console URLs.
	ConsoleBaseURL string
}

// Driver serves any number of simulated BMCs keyed by address. Unknown
// addresses are auto-provisioned on first touch with sane defaults, which
// keeps dev setups trivial ("register a machine with protocol=fake and go").
type Driver struct {
	mu   sync.Mutex
	bmcs map[string]*BMC

	// Delay simulates a slow BMC on every operation — used to exercise
	// heartbeats, lease renewal and the reaper without real hardware.
	Delay time.Duration
}

func New() *Driver {
	return &Driver{bmcs: map[string]*BMC{}}
}

// Add registers a scripted BMC under addr and returns it for tuning.
func (d *Driver) Add(addr string) *BMC {
	d.mu.Lock()
	defer d.mu.Unlock()
	serial := fmt.Sprintf("FAKE%06d", len(d.bmcs)+1)
	b := &BMC{
		Vendor:         "acme",
		Model:          "MammothSim 1000",
		Firmware:       "1.0.0-fake",
		Serial:         serial,
		Power:          bmc.PowerStateOff,
		ConsoleBaseURL: "https://fake.bmc/console",
		Hardware:       defaultHardware(serial),
		FirmwareList:   defaultFirmware(),
		SensorList:     defaultSensors(),
		SELList:        defaultSEL(),
		BIOS:           defaultBIOS(),
		EraseSupport:   true,
	}
	d.bmcs[addr] = b
	return b
}

// defaultHardware scripts a plausible two-NVMe + one-HDD server so the
// discovery flow, acceptance and spec-view tests have stable data.
func defaultHardware(serial string) *bmc.HardwareView {
	return &bmc.HardwareView{
		SerialNumber: serial,
		CPU:          bmc.CPUView{Model: "Fake Xeon 6542Y", Cores: 32},
		MemoryBytes:  128 * 1024 * 1024 * 1024,
		Disks: []bmc.DiskView{
			{Name: "nvme0n1", Serial: "S6XPN0001", SizeBytes: 1920383410176, Medium: "ssd", Protocol: "nvme"},
			{Name: "nvme1n1", Serial: "S6XPN0002", SizeBytes: 1920383410176, Medium: "ssd", Protocol: "nvme"},
			{Name: "sda", Serial: "GIM256_0001", SizeBytes: 4000787030016, Medium: "hdd", Protocol: "sata"},
		},
		NICs: []bmc.NICView{
			{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01", SpeedMbps: 25000, LinkUp: true, PCIAddress: "0000:0b:00.0"},
			{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02", SpeedMbps: 25000, LinkUp: true, PCIAddress: "0000:0c:00.0"},
		},
		Coverage: bmc.CoverageFull,
	}
}

// defaultBIOS scripts a plausible attribute table so the bios read/write
// path has stable data.
func defaultBIOS() map[string]any {
	return map[string]any{
		"BootMode":           "UEFI",
		"SrIovEnable":        false,
		"VTdSupport":         true,
		"PowerRestorePolicy": "LastState",
	}
}

// defaultFirmware scripts the controller's own firmware entries so
// discovery and the machine firmware view have stable data.
func defaultFirmware() []bmc.FirmwareComponent {
	return []bmc.FirmwareComponent{
		{ID: "BMC", Name: "MammothSim iBMC", Version: "1.0.0-fake"},
		{ID: "BIOS", Name: "MammothSim BIOS", Version: "5.49-fake"},
	}
}

// defaultSensors scripts a plausible health snapshot so the health view has
// stable data: two fans, two temperature zones, one power supply, one
// voltage rail.
func defaultSensors() []bmc.SensorReading {
	return []bmc.SensorReading{
		{Name: "Fan 1", Reading: 6600, Unit: "RPM", State: bmc.SensorOK},
		{Name: "Fan 2", Reading: 6100, Unit: "RPM", State: bmc.SensorOK},
		{Name: "CPU Temp", Reading: 46, Unit: "Celsius", State: bmc.SensorOK},
		{Name: "Inlet Temp", Reading: 24, Unit: "Celsius", State: bmc.SensorOK},
		{Name: "PSU 1", Reading: 210, Unit: "Watts", State: bmc.SensorOK},
		{Name: "+3.3V", Reading: 3.29, Unit: "Volts", State: bmc.SensorOK},
	}
}

// defaultSEL scripts a couple of log records so the SEL view has stable
// data.
func defaultSEL() []bmc.SELEntry {
	return []bmc.SELEntry{
		{ID: "1", Timestamp: "2026-09-24T08:00:00Z", Severity: "ok", Message: "System boot completed"},
		{ID: "2", Timestamp: "2026-09-24T08:15:00Z", Severity: "warning", Message: "Redundancy lost: PSU 2 removed"},
	}
}

func (d *Driver) get(addr string, op string) (*BMC, error) {
	d.mu.Lock()
	b, ok := d.bmcs[addr]
	delay := d.Delay
	d.mu.Unlock()
	if !ok {
		b = d.Add(addr)
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if err := b.fail(op); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *BMC) fail(op string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.FailOps == nil {
		return nil
	}
	if err, ok := b.FailOps[op]; ok && err != nil {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op, Detail: "scripted failure"}
	}
	return nil
}

func (d *Driver) Name() bmc.Protocol { return bmc.ProtocolFake }

func (d *Driver) Probe(_ context.Context, addr string, _ bmc.Credentials) (bmc.BMCInfo, error) {
	b, err := d.get(addr, "probe")
	if err != nil {
		return bmc.BMCInfo{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return bmc.BMCInfo{
		Protocol:        bmc.ProtocolFake,
		Vendor:          b.Vendor,
		Model:           b.Model,
		FirmwareVersion: b.Firmware,
		SerialNumber:    b.Serial,
		PowerState:      b.Power,
	}, nil
}

func (d *Driver) PowerState(_ context.Context, addr string, _ bmc.Credentials) (bmc.PowerState, error) {
	b, err := d.get(addr, "power_state")
	if err != nil {
		return bmc.PowerStateUnknown, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Power, nil
}

func (d *Driver) SetPower(_ context.Context, addr string, _ bmc.Credentials, action bmc.PowerAction) error {
	b, err := d.get(addr, "set_power")
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch action {
	case bmc.PowerOn:
		b.Power = bmc.PowerStateOn
	case bmc.PowerOff, bmc.SoftOff:
		b.Power = bmc.PowerStateOff
		b.BootOnce = false
	case bmc.HardReboot, bmc.SoftReboot:
		b.Power = bmc.PowerStateOn
	case bmc.Cycle:
		b.Power = bmc.PowerStateOn
	default:
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_power", Detail: fmt.Sprintf("unknown action %q", action)}
	}
	return nil
}

func (d *Driver) SetBootDevice(_ context.Context, addr string, _ bmc.Credentials, dev bmc.BootDevice, once bool) error {
	b, err := d.get(addr, "set_boot_device")
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch dev {
	case bmc.BootPXE, bmc.BootDisk, bmc.BootCDROM, bmc.BootBIOS:
		b.BootDevice = dev
		b.BootOnce = once
		return nil
	default:
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_boot_device", Detail: fmt.Sprintf("unknown device %q", dev)}
	}
}

func (d *Driver) MountMedia(_ context.Context, addr string, _ bmc.Credentials, img bmc.MediaImage) error {
	b, err := d.get(addr, "mount_media")
	if err != nil {
		return err
	}
	if _, err := url.Parse(img.URL); err != nil || !strings.Contains(img.URL, "://") {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: "mount_media", Detail: "invalid image url"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Media = append(b.Media, img)
	return nil
}

func (d *Driver) EjectMedia(_ context.Context, addr string, _ bmc.Credentials, img bmc.MediaImage) error {
	b, err := d.get(addr, "eject_media")
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var kept []bmc.MediaImage
	for _, m := range b.Media {
		if m.URL != img.URL {
			kept = append(kept, m)
		}
	}
	b.Media = kept
	return nil
}

func (d *Driver) ConsoleURL(_ context.Context, addr string, _ bmc.Credentials) (string, error) {
	b, err := d.get(addr, "console_url")
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("%s/%s?token=one-time", b.ConsoleBaseURL, strings.ReplaceAll(addr, ":", "-")), nil
}

// CreateVolume implements the bmc.VolumeCreator capability: declarative
// hardware RAID on the simulated controller. The logical drive surfaces in
// CollectInventory under the declared volume name. Idempotent by name.
func (d *Driver) CreateVolume(_ context.Context, addr string, _ bmc.Credentials, spec bmc.VolumeSpec) (string, error) {
	b, err := d.get(addr, "create_volume")
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, v := range b.Volumes {
		if v.Name == spec.Name {
			return spec.Name, nil // already exists — idempotent
		}
	}
	b.Volumes = append(b.Volumes, spec)
	// The logical drive appears as a fresh disk; fake capacity is fixed.
	if b.Hardware != nil {
		b.Hardware.Disks = append(b.Hardware.Disks, bmc.DiskView{
			Name: spec.Name, SizeBytes: 999922148352, Protocol: "raid",
		})
	}
	return spec.Name, nil
}

// PhysicalDrives implements the bmc.PhysicalDriveEnumerator capability: the
// fake presents its fixture hardware as the RAID member pool.
func (d *Driver) PhysicalDrives(_ context.Context, addr string, _ bmc.Credentials) ([]bmc.DiskView, error) {
	b, err := d.get(addr, "physical_drives")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Hardware == nil {
		return nil, nil
	}
	return append([]bmc.DiskView(nil), b.Hardware.Disks...), nil
}

// FirmwareInventory reports the scripted firmware inventory (the
// FirmwareInventoryProvider optional capability).
func (d *Driver) FirmwareInventory(_ context.Context, addr string, _ bmc.Credentials) ([]bmc.FirmwareComponent, error) {
	b, err := d.get(addr, "firmware_inventory")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bmc.FirmwareComponent(nil), b.FirmwareList...), nil
}

// BiosAttributes reports the scripted BIOS attribute table (BiosSetter
// read side).
func (d *Driver) BiosAttributes(_ context.Context, addr string, _ bmc.Credentials) (map[string]any, error) {
	b, err := d.get(addr, "bios_attributes")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]any, len(b.BIOS))
	for k, v := range b.BIOS {
		out[k] = v
	}
	return out, nil
}

// SetBiosAttributes applies the requested values immediately, rejecting
// unknown attribute names the way a controller's attribute registry does.
func (d *Driver) SetBiosAttributes(_ context.Context, addr string, _ bmc.Credentials, attrs map[string]any) error {
	b, err := d.get(addr, "set_bios_attributes")
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range attrs {
		if _, ok := b.BIOS[k]; !ok {
			return &bmc.Error{Kind: bmc.KindProtocolError, Op: "set_bios_attributes",
				Detail: "unknown BIOS attribute: " + k}
		}
	}
	for k, v := range attrs {
		b.BIOS[k] = v
	}
	return nil
}

// SecureErase implements the bmc.DriveEraser capability: record the erased
// serials. All serials must be present in the scripted hardware before
// anything is marked erased — the resolve-before-touching contract.
func (d *Driver) SecureErase(_ context.Context, addr string, _ bmc.Credentials, serials []string) ([]bmc.SanitizeResult, error) {
	b, err := d.get(addr, "secure_erase")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.EraseSupport {
		return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "secure_erase",
			Detail: "controller drives carry no secure-erase action"}
	}
	if b.Hardware != nil {
		live := map[string]bool{}
		for _, disk := range b.Hardware.Disks {
			if disk.Serial != "" {
				live[disk.Serial] = true
			}
		}
		var missing []string
		for _, s := range serials {
			if !live[s] {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: "secure_erase",
				Detail: "serials not found on the controller — nothing erased: " + strings.Join(missing, ",")}
		}
	}
	results := make([]bmc.SanitizeResult, 0, len(serials))
	for _, s := range serials {
		b.ErasedSerials = append(b.ErasedSerials, s)
		results = append(results, bmc.SanitizeResult{Serial: s, Method: "fake-purge"})
	}
	return results, nil
}

// Health reports the scripted health snapshot (the HealthProvider optional
// capability): the scripted sensor list plus the chassis power state
// (PowerScripted overrides, Power falls through) and the worst sensor
// state as the overall verdict.
func (d *Driver) Health(_ context.Context, addr string, _ bmc.Credentials) (bmc.HealthView, error) {
	b, err := d.get(addr, "health")
	if err != nil {
		return bmc.HealthView{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sensors := append([]bmc.SensorReading(nil), b.SensorList...)
	power := b.PowerScripted
	if power == "" {
		power = b.Power
	}
	overall := bmc.SensorOK
	seen := false
	for _, s := range sensors {
		seen = seen || s.State != bmc.SensorUnknown
		if s.State == bmc.SensorCritical {
			overall = bmc.SensorCritical
		} else if s.State == bmc.SensorWarning && overall != bmc.SensorCritical {
			overall = bmc.SensorWarning
		}
	}
	if !seen {
		overall = bmc.SensorUnknown
	}
	return bmc.HealthView{PowerState: power, Health: overall, Sensors: sensors}, nil
}

// SystemEventLog reports the scripted system event log (the SELReader
// optional capability), newest first — the fake list is used as-is because
// tests script it in presentation order.
func (d *Driver) SystemEventLog(_ context.Context, addr string, _ bmc.Credentials) ([]bmc.SELEntry, error) {
	b, err := d.get(addr, "sel")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bmc.SELEntry(nil), b.SELList...), nil
}

func (d *Driver) CollectInventory(_ context.Context, addr string, _ bmc.Credentials) (bmc.HardwareView, error) {
	b, err := d.get(addr, "collect_inventory")
	if err != nil {
		return bmc.HardwareView{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Hardware != nil {
		return *b.Hardware, nil
	}
	return bmc.HardwareView{SerialNumber: b.Serial, Coverage: bmc.CoveragePartial,
		CoverageNotes: []string{"simulator without scripted hardware"}}, nil
}
