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

type errFake struct{}

func (errFake) Error() string { return "scripted" }
