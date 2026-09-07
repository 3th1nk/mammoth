// Package redfish implements bmc.Driver over the Redfish standard
// (github.com/stmcginnis/gofish). No vendor SDKs: standard HTTP + the
// published schema, OEM extensions enter later behind optional interfaces
// (docs/07-bmc.md §5).
package redfish

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

type Driver struct {
	// Insecure skips TLS verification (self-signed BMC certs are the norm).
	Insecure bool
	// Timeout bounds a single HTTP exchange against the BMC.
	Timeout time.Duration
}

func New(insecure bool, timeout time.Duration) *Driver {
	return &Driver{Insecure: insecure, Timeout: timeout}
}

func (d *Driver) Name() bmc.Protocol { return bmc.ProtocolRedfish }

// connect establishes an authenticated Redfish session.
func (d *Driver) connect(ctx context.Context, addr string, cred bmc.Credentials) (*gofish.APIClient, error) {
	client, err := gofish.ConnectContext(ctx, gofish.ClientConfig{
		Endpoint:   normalizeHost(addr),
		Username:   cred.Username,
		Password:   cred.Password,
		Insecure:   d.Insecure,
		HTTPClient: makeHTTPClient(d.Timeout),
	})
	if err != nil {
		return nil, bmc.Classify("connect", err)
	}
	return client, nil
}

func (d *Driver) Probe(ctx context.Context, addr string, cred bmc.Credentials) (bmc.BMCInfo, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return bmc.BMCInfo{}, err
	}
	defer c.Logout()

	info := bmc.BMCInfo{Protocol: bmc.ProtocolRedfish, PowerState: bmc.PowerStateUnknown}

	if systems, err := c.Service.Systems(); err == nil && len(systems) > 0 {
		sys := systems[0]
		info.Vendor = strings.ToLower(sys.Manufacturer)
		info.Model = sys.Model
		info.SerialNumber = sys.SerialNumber
		switch redfish.PowerState(sys.PowerState) {
		case redfish.OnPowerState:
			info.PowerState = bmc.PowerStateOn
		case redfish.OffPowerState:
			info.PowerState = bmc.PowerStateOff
		}
	}
	if mgrs, err := c.Service.Managers(); err == nil && len(mgrs) > 0 {
		info.FirmwareVersion = mgrs[0].FirmwareVersion
	}
	if info.Vendor == "" && info.Model == "" && info.FirmwareVersion == "" {
		return info, &bmc.Error{Kind: bmc.KindProtocolError, Op: "probe",
			Detail: "service root answered but carries no system/manager data"}
	}
	return info, nil
}

func (d *Driver) PowerState(ctx context.Context, addr string, cred bmc.Credentials) (bmc.PowerState, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return bmc.PowerStateUnknown, err
	}
	defer c.Logout()

	systems, err := c.Service.Systems()
	if err != nil {
		return bmc.PowerStateUnknown, bmc.Classify("power_state", err)
	}
	if len(systems) == 0 {
		return bmc.PowerStateUnknown, &bmc.Error{Kind: bmc.KindProtocolError, Op: "power_state", Detail: "no computer system resource"}
	}
	switch redfish.PowerState(systems[0].PowerState) {
	case redfish.OnPowerState:
		return bmc.PowerStateOn, nil
	case redfish.OffPowerState:
		return bmc.PowerStateOff, nil
	default:
		return bmc.PowerStateUnknown, nil
	}
}

func (d *Driver) SetPower(ctx context.Context, addr string, cred bmc.Credentials, action bmc.PowerAction) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}
	defer c.Logout()

	resetType, ok := resetTypeFor(action)
	if !ok {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_power",
			Detail: fmt.Sprintf("action %q has no Redfish ResetType mapping", action)}
	}
	systems, err := c.Service.Systems()
	if err != nil {
		return bmc.Classify("set_power", err)
	}
	if len(systems) == 0 {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: "set_power", Detail: "no computer system resource"}
	}
	if err := systems[0].Reset(resetType); err != nil {
		return bmc.Classify("set_power", err)
	}
	return nil
}

// resetTypeFor maps unified power actions onto standard Redfish ResetType.
func resetTypeFor(a bmc.PowerAction) (redfish.ResetType, bool) {
	switch a {
	case bmc.PowerOn:
		return redfish.ForceOnResetType, true
	case bmc.PowerOff:
		return redfish.ForceOffResetType, true
	case bmc.SoftOff:
		return redfish.GracefulShutdownResetType, true
	case bmc.HardReboot:
		return redfish.ForceRestartResetType, true
	case bmc.SoftReboot:
		return redfish.GracefulRestartResetType, true
	case bmc.Cycle:
		return redfish.PowerCycleResetType, true
	default:
		return "", false
	}
}

func (d *Driver) SetBootDevice(ctx context.Context, addr string, cred bmc.Credentials, dev bmc.BootDevice, once bool) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}
	defer c.Logout()

	target, ok := bootTargetFor(dev)
	if !ok {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_boot_device",
			Detail: fmt.Sprintf("device %q has no Redfish mapping", dev)}
	}
	systems, err := c.Service.Systems()
	if err != nil {
		return bmc.Classify("set_boot_device", err)
	}
	if len(systems) == 0 {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: "set_boot_device", Detail: "no computer system resource"}
	}
	sys := systems[0]
	boot := sys.Boot
	boot.BootSourceOverrideTarget = target
	if once {
		boot.BootSourceOverrideEnabled = redfish.OnceBootSourceOverrideEnabled
	} else {
		boot.BootSourceOverrideEnabled = redfish.ContinuousBootSourceOverrideEnabled
	}
	if err := sys.SetBoot(boot); err != nil {
		return bmc.Classify("set_boot_device", err)
	}
	return nil
}

func bootTargetFor(d bmc.BootDevice) (redfish.BootSourceOverrideTarget, bool) {
	switch d {
	case bmc.BootPXE:
		return redfish.PxeBootSourceOverrideTarget, true
	case bmc.BootDisk:
		return redfish.HddBootSourceOverrideTarget, true
	case bmc.BootCDROM:
		return redfish.CdBootSourceOverrideTarget, true
	case bmc.BootBIOS:
		return redfish.BiosSetupBootSourceOverrideTarget, true
	default:
		return "", false
	}
}

func (d *Driver) MountMedia(ctx context.Context, addr string, cred bmc.Credentials, img bmc.MediaImage) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}
	defer c.Logout()

	vm, err := firstVirtualMedia(c)
	if err != nil {
		return err
	}
	if !vm.SupportsMediaInsert {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "mount_media", Detail: "virtual media insert not advertised"}
	}
	if err := vm.InsertMedia(img.URL, true, true); err != nil {
		return bmc.Classify("mount_media", err)
	}
	return nil
}

func (d *Driver) EjectMedia(ctx context.Context, addr string, cred bmc.Credentials) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}
	defer c.Logout()

	vms, err := virtualMediaList(c)
	if err != nil {
		return err
	}
	if len(vms) == 0 {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "eject_media", Detail: "no virtual media resource"}
	}
	var firstErr error
	for _, vm := range vms {
		if !vm.Inserted {
			continue
		}
		if err := vm.EjectMedia(); err != nil && firstErr == nil {
			firstErr = bmc.Classify("eject_media", err)
		}
	}
	return firstErr
}

func firstVirtualMedia(c *gofish.APIClient) (*redfish.VirtualMedia, error) {
	vms, err := virtualMediaList(c)
	if err != nil {
		return nil, err
	}
	if len(vms) == 0 {
		return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "mount_media", Detail: "no virtual media resource"}
	}
	return vms[0], nil
}

func virtualMediaList(c *gofish.APIClient) ([]*redfish.VirtualMedia, error) {
	managers, err := c.Service.Managers()
	if err != nil {
		return nil, bmc.Classify("virtual_media", err)
	}
	var all []*redfish.VirtualMedia
	for _, m := range managers {
		vms, err := m.VirtualMedia()
		if err != nil {
			continue
		}
		all = append(all, vms...)
	}
	return all, nil
}

// ConsoleURL is OEM territory: no standard Redfish KVM resource exists. Known
// OEM mappings enter behind an OEMExtensions interface; until then report
// unsupported so callers degrade gracefully (docs/07-bmc.md §5).
func (d *Driver) ConsoleURL(_ context.Context, _ string, _ bmc.Credentials) (string, error) {
	return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: "console_url",
		Detail: "no OEM KVM mapping for this vendor yet"}
}

// normalizeHost accepts bare hosts, host:port and URL forms.
func normalizeHost(addr string) string {
	if strings.Contains(addr, "://") {
		return strings.TrimSuffix(addr, "/")
	}
	return "https://" + addr
}

func makeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}
