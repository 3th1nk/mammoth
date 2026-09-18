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

type errFake struct{}

func (errFake) Error() string { return "scripted" }
