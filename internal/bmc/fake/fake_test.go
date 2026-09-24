package fake

import (
	"context"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// The firmware inventory capability: default data on auto-provisioned
// controllers, scriptable via the exported field, failure-injectable via
// FailOps, and absent on drivers that don't implement the interface.
func TestFirmwareInventory(t *testing.T) {
	d := New()

	t.Run("implements the optional capability", func(t *testing.T) {
		if _, ok := bmc.Driver(d).(bmc.FirmwareInventoryProvider); !ok {
			t.Fatalf("fake must implement FirmwareInventoryProvider")
		}
	})

	t.Run("auto-provisioned card reports defaults", func(t *testing.T) {
		got, err := d.FirmwareInventory(context.Background(), "fake://fw-a", bmc.Credentials{})
		if err != nil {
			t.Fatalf("FirmwareInventory: %v", err)
		}
		if len(got) != 2 || got[0].ID != "BMC" || got[1].ID != "BIOS" {
			t.Fatalf("unexpected defaults: %+v", got)
		}
	})

	t.Run("scripted list and failure injection", func(t *testing.T) {
		card := d.Add("fake://fw-b")
		card.FirmwareList = []bmc.FirmwareComponent{{ID: "CPLD", Version: "3.1"}}
		got, err := d.FirmwareInventory(context.Background(), "fake://fw-b", bmc.Credentials{})
		if err != nil || len(got) != 1 || got[0].ID != "CPLD" {
			t.Fatalf("scripted list wrong: %+v %v", got, err)
		}
		card.FailOps = map[string]error{"firmware_inventory": errFake{}}
		if _, err := d.FirmwareInventory(context.Background(), "fake://fw-b", bmc.Credentials{}); err == nil {
			t.Fatalf("expected scripted failure")
		}
	})
}

// The BIOS two-stage contract's fake side: live table read, unknown-name
// rejection, immediate apply, scripted failure.
func TestBiosSetter(t *testing.T) {
	d := New()
	ctx := context.Background()
	if _, ok := bmc.Driver(d).(bmc.BiosSetter); !ok {
		t.Fatalf("fake must implement BiosSetter")
	}

	attrs, err := d.BiosAttributes(ctx, "fake://bios-a", bmc.Credentials{})
	if err != nil || attrs["BootMode"] != "UEFI" {
		t.Fatalf("defaults wrong: %v %v", attrs, err)
	}
	if err := d.SetBiosAttributes(ctx, "fake://bios-a", bmc.Credentials{},
		map[string]any{"SrIovEnable": true, "NoSuchAttr": 1}); err == nil {
		t.Fatalf("unknown attribute must be rejected")
	}
	if err := d.SetBiosAttributes(ctx, "fake://bios-a", bmc.Credentials{},
		map[string]any{"SrIovEnable": true}); err != nil {
		t.Fatalf("SetBiosAttributes: %v", err)
	}
	attrs, _ = d.BiosAttributes(ctx, "fake://bios-a", bmc.Credentials{})
	if attrs["SrIovEnable"] != true {
		t.Fatalf("write not visible on read: %+v", attrs)
	}

	card := d.Add("fake://bios-b")
	card.FailOps = map[string]error{"set_bios_attributes": errFake{}}
	if err := d.SetBiosAttributes(ctx, "fake://bios-b", bmc.Credentials{},
		map[string]any{"BootMode": "Legacy"}); err == nil {
		t.Fatalf("expected scripted failure")
	}
}

// The secure-erase contract's fake side: capability scripting, serial
// resolution against the scripted hardware, erased-serial recording.
func TestSecureErase(t *testing.T) {
	d := New()
	ctx := context.Background()
	if _, ok := bmc.Driver(d).(bmc.DriveEraser); !ok {
		t.Fatalf("fake must implement DriveEraser")
	}

	// Default controllers support secure erase (drive purge actions are the
	// norm); scripting EraseSupport=false simulates a controller that
	// cannot.
	nope := d.Add("fake://erase-none")
	nope.EraseSupport = false
	if _, err := d.SecureErase(ctx, "fake://erase-none", bmc.Credentials{}, []string{"S6XPN0001"}); err == nil {
		t.Fatalf("erase without EraseSupport must be unsupported")
	}

	card := d.Add("fake://erase-b")
	card.EraseSupport = true
	if _, err := d.SecureErase(ctx, "fake://erase-b", bmc.Credentials{}, []string{"GHOST"}); err == nil {
		t.Fatalf("unknown serial must be rejected before anything is erased")
	}
	if len(card.ErasedSerials) != 0 {
		t.Fatalf("nothing erased on rejection: %v", card.ErasedSerials)
	}
	res, err := d.SecureErase(ctx, "fake://erase-b", bmc.Credentials{}, []string{"S6XPN0001"})
	if err != nil || len(res) != 1 || res[0].Serial != "S6XPN0001" {
		t.Fatalf("erase failed: %v %v", res, err)
	}
	if len(card.ErasedSerials) != 1 || card.ErasedSerials[0] != "S6XPN0001" {
		t.Fatalf("erased serials not recorded: %v", card.ErasedSerials)
	}

	card.FailOps = map[string]error{"secure_erase": errFake{}}
	if _, err := d.SecureErase(ctx, "fake://erase-b", bmc.Credentials{}, []string{"S6XPN0001"}); err == nil {
		t.Fatalf("expected scripted failure")
	}
}

type errFake struct{}

func (errFake) Error() string { return "scripted" }

// The health + SEL read capabilities: default data on auto-provisioned
// controllers, scriptable via the exported fields, failure-injectable via
// FailOps (docs/07-bmc.md §6.3).
func TestHealthAndSEL(t *testing.T) {
	d := New()
	ctx := context.Background()
	if _, ok := bmc.Driver(d).(bmc.HealthProvider); !ok {
		t.Fatalf("fake must implement HealthProvider")
	}
	if _, ok := bmc.Driver(d).(bmc.SELReader); !ok {
		t.Fatalf("fake must implement SELReader")
	}

	t.Run("auto-provisioned card reports defaults", func(t *testing.T) {
		got, err := d.Health(ctx, "fake://health-a", bmc.Credentials{})
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if got.Health != bmc.SensorOK || len(got.Sensors) == 0 {
			t.Fatalf("unexpected default health: %+v", got)
		}
		if got.PowerState != bmc.PowerStateOff {
			t.Fatalf("default power state must fall through to Power: %+v", got)
		}
		sel, err := d.SystemEventLog(ctx, "fake://health-a", bmc.Credentials{})
		if err != nil || len(sel) == 0 {
			t.Fatalf("unexpected default SEL: %+v %v", sel, err)
		}
	})

	t.Run("scripted sensors drive the overall verdict", func(t *testing.T) {
		card := d.Add("fake://health-b")
		card.SensorList = []bmc.SensorReading{
			{Name: "Fan 1", Reading: 6000, Unit: "RPM", State: bmc.SensorOK},
			{Name: "CPU Temp", Reading: 92, Unit: "Celsius", State: bmc.SensorCritical},
		}
		got, err := d.Health(ctx, "fake://health-b", bmc.Credentials{})
		if err != nil || got.Health != bmc.SensorCritical {
			t.Fatalf("critical sensor must dominate: %+v %v", got, err)
		}
		card.PowerScripted = bmc.PowerStateOn
		got, _ = d.Health(ctx, "fake://health-b", bmc.Credentials{})
		if got.PowerState != bmc.PowerStateOn {
			t.Fatalf("PowerScripted must override: %+v", got)
		}
	})

	t.Run("scripted SEL and failure injection", func(t *testing.T) {
		card := d.Add("fake://health-c")
		card.SELList = []bmc.SELEntry{{ID: "9", Severity: "warning", Message: "redundancy lost"}}
		sel, err := d.SystemEventLog(ctx, "fake://health-c", bmc.Credentials{})
		if err != nil || len(sel) != 1 || sel[0].ID != "9" {
			t.Fatalf("scripted SEL wrong: %+v %v", sel, err)
		}
		card.FailOps = map[string]error{"health": errFake{}}
		if _, err := d.Health(ctx, "fake://health-c", bmc.Credentials{}); err == nil {
			t.Fatalf("expected scripted health failure")
		}
	})
}
