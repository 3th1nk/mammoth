package fake

import (
	"context"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// The fake's RAID volume lifecycle (docs/07-bmc.md §6.7): creation surfaces
// the logical drive in inventory and consumes its members from the physical
// pool; deletion frees them again. Idempotent creation by name; unknown
// delete rejects without touching anything.
func TestVolumeLifecycle(t *testing.T) {
	d := New()
	ctx := context.Background()
	addr := "fake://raid-a"
	cred := bmc.Credentials{}

	name, err := d.CreateVolume(ctx, addr, cred, bmc.VolumeSpec{
		Name: "os", RAIDType: "RAID1", MemberSerials: []string{"S6XPN0001", "S6XPN0002"},
	})
	if err != nil || name != "os" {
		t.Fatalf("create: %q, %v", name, err)
	}

	t.Run("re-create by name is idempotent", func(t *testing.T) {
		again, err := d.CreateVolume(ctx, addr, cred, bmc.VolumeSpec{
			Name: "os", RAIDType: "RAID1", MemberSerials: []string{"S6XPN0001", "S6XPN0002"},
		})
		if err != nil || again != "os" {
			t.Fatalf("idempotent create: %q, %v", again, err)
		}
	})

	t.Run("volumes list carries members and capacity", func(t *testing.T) {
		if _, ok := bmc.Driver(d).(bmc.VolumeEnumerator); !ok {
			t.Fatalf("fake must implement VolumeEnumerator")
		}
		volumes, err := d.Volumes(ctx, addr, cred)
		if err != nil || len(volumes) != 1 {
			t.Fatalf("volumes: %+v, %v", volumes, err)
		}
		v := volumes[0]
		if v.Name != "os" || v.RAIDType != "RAID1" || v.SizeBytes == 0 {
			t.Errorf("volume view = %+v", v)
		}
		if len(v.MemberSerials) != 2 || v.MemberSerials[0] != "S6XPN0001" {
			t.Errorf("member serials = %v", v.MemberSerials)
		}
	})

	t.Run("physical pool excludes the logical drive", func(t *testing.T) {
		pool, err := d.PhysicalDrives(ctx, addr, cred)
		if err != nil {
			t.Fatal(err)
		}
		for _, disk := range pool {
			if disk.Name == "os" {
				t.Fatalf("logical drive leaked into the physical pool: %+v", pool)
			}
		}
		if len(pool) != 3 {
			t.Errorf("pool = %d drives, want the 3 fixture disks", len(pool))
		}
	})

	t.Run("inventory still presents the volume", func(t *testing.T) {
		hw, err := d.CollectInventory(ctx, addr, cred)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, disk := range hw.Disks {
			if disk.Name == "os" && disk.Protocol == "raid" {
				found = true
			}
		}
		if !found {
			t.Errorf("volume missing from inventory: %+v", hw.Disks)
		}
	})

	t.Run("delete frees the members", func(t *testing.T) {
		if _, ok := bmc.Driver(d).(bmc.VolumeDeleter); !ok {
			t.Fatalf("fake must implement VolumeDeleter")
		}
		if err := d.DeleteVolume(ctx, addr, cred, "os"); err != nil {
			t.Fatal(err)
		}
		pool, err := d.PhysicalDrives(ctx, addr, cred)
		if err != nil || len(pool) != 3 {
			t.Fatalf("pool after delete = %d drives, %v — members must return", len(pool), err)
		}
		hw, err := d.CollectInventory(ctx, addr, cred)
		if err != nil {
			t.Fatal(err)
		}
		for _, disk := range hw.Disks {
			if disk.Name == "os" {
				t.Fatalf("deleted volume still in inventory: %+v", hw.Disks)
			}
		}
	})

	t.Run("unknown volume rejects", func(t *testing.T) {
		if err := d.DeleteVolume(ctx, addr, cred, "ghost"); err == nil {
			t.Fatal("unknown delete must reject")
		}
	})
}
