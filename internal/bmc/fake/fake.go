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
			{Name: "nvme0", Serial: "S6XPN0001", SizeBytes: 1920383410176, Medium: "ssd", Protocol: "nvme"},
			{Name: "nvme1", Serial: "S6XPN0002", SizeBytes: 1920383410176, Medium: "ssd", Protocol: "nvme"},
			{Name: "sda", Serial: "GIM256_0001", SizeBytes: 4000787030016, Medium: "hdd", Protocol: "sata"},
		},
		NICs: []bmc.NICView{
			{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01", SpeedMbps: 25000, LinkUp: true, PCIAddress: "0000:0b:00.0"},
			{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02", SpeedMbps: 25000, LinkUp: true, PCIAddress: "0000:0c:00.0"},
		},
		Coverage: bmc.CoverageFull,
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
