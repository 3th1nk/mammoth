// Package ipmi implements the narrow IPMI subset Mammoth needs over a pure-Go
// stack (github.com/vmware/goipmi), honoring decision D5: no ipmitool shell
// out, no system package dependency. Scope (docs/07-bmc.md §2): chassis
// power, boot device, identity probe; virtual media and KVM are OEM commands
// and reported as unsupported until vendor drivers add them.
package ipmi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	goipmi "github.com/vmware/goipmi"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// DefaultPort is the standard IPMI-over-LAN port (RMCP).
const DefaultPort = 623

type Driver struct {
	// Interface selects the transport dialect: lanplus (default) | lan.
	Interface string
	// Timeout bounds one request/response exchange.
	Timeout time.Duration
}

func New(iface string, timeout time.Duration) *Driver {
	if iface == "" {
		iface = "lanplus"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Driver{Interface: iface, Timeout: timeout}
}

func (d *Driver) Name() bmc.Protocol { return bmc.ProtocolIPMI }

// connect opens an authenticated IPMI session. Note: goipmi performs its own
// retries for the RAKP handshake; the timeout bounds each exchange.
func (d *Driver) connect(addr string, cred bmc.Credentials) (*goipmi.Client, error) {
	host, port := splitHostPort(addr)
	conn := &goipmi.Connection{
		Hostname:  host,
		Port:      port,
		Username:  cred.Username,
		Password:  cred.Password,
		Interface: d.Interface,
	}
	client, err := goipmi.NewClient(conn)
	if err != nil {
		return nil, bmc.Classify("connect", err)
	}
	if err := client.Open(); err != nil {
		return nil, bmc.Classify("connect", err)
	}
	return client, nil
}

func (d *Driver) Probe(ctx context.Context, addr string, cred bmc.Credentials) (bmc.BMCInfo, error) {
	info := bmc.BMCInfo{Protocol: bmc.ProtocolIPMI, PowerState: bmc.PowerStateUnknown}

	client, err := d.connect(addr, cred)
	if err != nil {
		return info, err
	}
	defer client.Close()

	devID, err := client.DeviceID()
	if err != nil {
		return info, bmc.Classify("probe", err)
	}
	info.Vendor = vendorName(uint32(devID.ManufacturerID))
	info.FirmwareVersion = fmt.Sprintf("%d.%02x", devID.FirmwareRevision1, devID.FirmwareRevision2)

	power, perr := d.powerState(ctx, client)
	if perr == nil {
		info.PowerState = power
	}
	return info, nil
}

func (d *Driver) powerState(_ context.Context, client *goipmi.Client) (bmc.PowerState, error) {
	res := &goipmi.ChassisStatusResponse{}
	req := &goipmi.Request{
		NetworkFunction: goipmi.NetworkFunctionChassis,
		Command:         goipmi.CommandChassisStatus,
		Data:            &goipmi.ChassisStatusRequest{},
	}
	if err := client.Send(req, res); err != nil {
		return bmc.PowerStateUnknown, bmc.Classify("power_state", err)
	}
	if res.IsSystemPowerOn() {
		return bmc.PowerStateOn, nil
	}
	return bmc.PowerStateOff, nil
}

func (d *Driver) PowerState(ctx context.Context, addr string, cred bmc.Credentials) (bmc.PowerState, error) {
	client, err := d.connect(addr, cred)
	if err != nil {
		return bmc.PowerStateUnknown, err
	}
	defer client.Close()
	return d.powerState(ctx, client)
}

func (d *Driver) SetPower(ctx context.Context, addr string, cred bmc.Credentials, action bmc.PowerAction) error {
	ctl, ok := controlFor(action)
	if !ok {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_power",
			Detail: fmt.Sprintf("action %q has no standard IPMI chassis control (use redfish or hard_reboot)", action)}
	}
	client, err := d.connect(addr, cred)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Control(ctl); err != nil {
		return bmc.Classify("set_power", err)
	}
	return nil
}

// controlFor maps unified actions onto standard IPMI chassis controls.
// IPMI has no graceful-restart command; soft_reboot stays unsupported rather
// than silently degrading to a hard reset (docs/07-bmc.md §1: unsupported is
// explicit so callers can pick a different path).
func controlFor(a bmc.PowerAction) (goipmi.ChassisControl, bool) {
	switch a {
	case bmc.PowerOn:
		return goipmi.ControlPowerUp, true
	case bmc.PowerOff:
		return goipmi.ControlPowerDown, true
	case bmc.SoftOff:
		return goipmi.ControlPowerAcpiSoft, true
	case bmc.HardReboot:
		return goipmi.ControlPowerHardReset, true
	case bmc.Cycle:
		return goipmi.ControlPowerCycle, true
	default:
		return 0, false
	}
}

// SetBootDevice writes the boot-flags parameter (IPMI 2.0 §28.12). goipmi's
// wrapper always sets the permanent bit, so the one-shot variant is issued
// directly: flag-valid + apply-to-next-boot-attempt.
func (d *Driver) SetBootDevice(_ context.Context, addr string, cred bmc.Credentials, dev bmc.BootDevice, once bool) error {
	ipmiDev, ok := bootDeviceFor(dev)
	if !ok {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_boot_device",
			Detail: fmt.Sprintf("device %q has no IPMI boot-flag mapping", dev)}
	}
	client, err := d.connect(addr, cred)
	if err != nil {
		return err
	}
	defer client.Close()

	flagValid := uint8(0x80)
	if once {
		// bit 6: boot options apply to the next boot attempt only.
		flagValid |= 0x40
	}
	data := []byte{flagValid, uint8(ipmiDev), 0x00, 0x00, 0x00}
	req := &goipmi.Request{
		NetworkFunction: goipmi.NetworkFunctionChassis,
		Command:         goipmi.CommandSetSystemBootOptions,
		Data: &goipmi.SetSystemBootOptionsRequest{
			Param: goipmi.BootParamBootFlags,
			Data:  data,
		},
	}
	if err := client.Send(req, &goipmi.SetSystemBootOptionsResponse{}); err != nil {
		return bmc.Classify("set_boot_device", err)
	}
	return nil
}

func bootDeviceFor(dev bmc.BootDevice) (goipmi.BootDevice, bool) {
	switch dev {
	case bmc.BootPXE:
		return goipmi.BootDevicePxe, true
	case bmc.BootDisk:
		return goipmi.BootDeviceDisk, true
	case bmc.BootCDROM:
		return goipmi.BootDeviceCdrom, true
	case bmc.BootBIOS:
		return goipmi.BootDeviceBios, true
	default:
		return 0, false
	}
}

// MountMedia: standard IPMI has no virtual media; every real implementation
// is a vendor OEM command (docs/07-bmc.md §2) — reported unsupported here,
// vendor drivers add OEM support incrementally.
func (d *Driver) MountMedia(_ context.Context, _ string, _ bmc.Credentials, _ bmc.MediaImage) error {
	return &bmc.Error{Kind: bmc.KindUnsupported, Op: "mount_media",
		Detail: "virtual media over IPMI is a vendor OEM command; not available on this driver"}
}

func (d *Driver) EjectMedia(_ context.Context, _ string, _ bmc.Credentials, _ bmc.MediaImage) error {
	return &bmc.Error{Kind: bmc.KindUnsupported, Op: "eject_media",
		Detail: "virtual media over IPMI is a vendor OEM command; not available on this driver"}
}

func (d *Driver) ConsoleURL(_ context.Context, _ string, _ bmc.Credentials) (string, error) {
	return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: "console_url",
		Detail: "IPMI offers serial-over-LAN only, no KVM URL"}
}

func (d *Driver) CollectInventory(_ context.Context, _ string, _ bmc.Credentials) (bmc.HardwareView, error) {
	return bmc.HardwareView{}, &bmc.Error{Kind: bmc.KindUnsupported, Op: "collect_inventory",
		Detail: "IPMI yields FRU/limited inventory; use the redfish probe"}
}

func splitHostPort(addr string) (string, int) {
	host, portStr, err := split(addr)
	port := DefaultPort
	if err == nil {
		if p, perr := strconv.Atoi(portStr); perr == nil {
			port = p
		}
		return host, port
	}
	return addr, port
}

func split(addr string) (host, port string, err error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return addr, "", fmt.Errorf("no port in %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

// vendorName maps a few common IANA enterprise IDs to vendor labels for
// metrics/logs; unknown ids fall back to the numeric form.
func vendorName(id uint32) string {
	switch id {
	case 674:
		return "dell"
	case 11, 232:
		return "hpe"
	case 7880, 39217:
		return "supermicro"
	case 19042, 20314:
		return "lenovo"
	case 4181:
		return "ami"
	default:
		return "iana:" + strconv.FormatUint(uint64(id), 10)
	}
}
