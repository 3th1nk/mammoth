package redfish

import (
	"testing"

	"github.com/stmcginnis/gofish/common"
	"github.com/stmcginnis/gofish/redfish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// The volume-first presentation rule (docs/05-inventory.md §2) is the risky
// logic of spec-level inventory: what the installer will see is what the
// user must see. These fixtures pin it down.
func TestMapStoragePresentationRule(t *testing.T) {
	newDrive := func(name string, size int64, media redfish.MediaType, proto string) *redfish.Drive {
		return &redfish.Drive{
			Entity:        common.Entity{Name: name},
			CapacityBytes: size,
			MediaType:     media,
			Protocol:      common.Protocol(proto),
		}
	}

	t.Run("raid controller presents volumes only", func(t *testing.T) {
		hw := bmc.HardwareView{Coverage: bmc.CoverageFull}
		mapStorage(&hw, []storageGroup{{
			name: "RAID Slot 1",
			volumes: []*redfish.Volume{
				{Entity: common.Entity{Name: "Logical Volume 1"}, CapacityBytes: 999},
				{Entity: common.Entity{Name: "Logical Volume 2"}, CapacityBytes: 1999},
			},
			drives: []*redfish.Drive{ // physical members must NOT leak into the view
				newDrive("Disk 0", 1000, redfish.SSDMediaType, "NVMe"),
				newDrive("Disk 1", 1000, redfish.SSDMediaType, "NVMe"),
			},
		}})
		if len(hw.Disks) != 2 {
			t.Fatalf("want 2 volume-disks, got %d: %+v", len(hw.Disks), hw.Disks)
		}
		for _, d := range hw.Disks {
			if d.Protocol != "raid" {
				t.Fatalf("volume disk protocol = %q, want raid", d.Protocol)
			}
			if d.Serial != "" {
				t.Fatalf("logical volume must not carry a physical serial: %+v", d)
			}
		}
		if hw.Coverage != bmc.CoverageFull {
			t.Fatalf("raid presentation must not downgrade coverage: %v", hw.Coverage)
		}
	})

	t.Run("hba controller presents physical drives", func(t *testing.T) {
		hw := bmc.HardwareView{Coverage: bmc.CoverageFull}
		mapStorage(&hw, []storageGroup{{
			name: "HBA",
			drives: []*redfish.Drive{
				newDrive("nvme0", 1920383410176, redfish.SSDMediaType, "NVMe"),
				newDrive("sda", 4000787030016, redfish.HDDMediaType, "SATA"),
			},
		}})
		if len(hw.Disks) != 2 {
			t.Fatalf("want 2 physical disks, got %d", len(hw.Disks))
		}
		if hw.Disks[0].Medium != "ssd" || hw.Disks[0].Protocol != "nvme" {
			t.Fatalf("nvme mapping wrong: %+v", hw.Disks[0])
		}
		if hw.Disks[1].Medium != "hdd" || hw.Disks[1].Protocol != "sata" {
			t.Fatalf("sata-hdd mapping wrong: %+v", hw.Disks[1])
		}
	})

	t.Run("mixed controllers combine volumes and drives", func(t *testing.T) {
		hw := bmc.HardwareView{Coverage: bmc.CoverageFull}
		mapStorage(&hw, []storageGroup{
			{name: "RAID", volumes: []*redfish.Volume{{Entity: common.Entity{Name: "LD1"}, CapacityBytes: 999}}},
			{name: "HBA", drives: []*redfish.Drive{newDrive("nvme0", 1000, redfish.SSDMediaType, "NVMe")}},
		})
		if len(hw.Disks) != 2 || hw.Disks[0].Protocol != "raid" || hw.Disks[1].Protocol != "nvme" {
			t.Fatalf("mixed mapping wrong: %+v", hw.Disks)
		}
	})

	t.Run("enumeration failure marks partial, not failure", func(t *testing.T) {
		hw := bmc.HardwareView{Coverage: bmc.CoverageFull}
		mapStorage(&hw, []storageGroup{{name: "Broken", err: errTestDriveEnum}})
		if len(hw.Disks) != 0 {
			t.Fatalf("failed controller must contribute no disks")
		}
		if hw.Coverage != bmc.CoveragePartial || len(hw.CoverageNotes) != 1 {
			t.Fatalf("want partial coverage with one note, got %v %v", hw.Coverage, hw.CoverageNotes)
		}
	})
}

var errTestDriveEnum = errFakeDriveEnum{}

type errFakeDriveEnum struct{}

func (errFakeDriveEnum) Error() string { return "drive enumeration broken" }
