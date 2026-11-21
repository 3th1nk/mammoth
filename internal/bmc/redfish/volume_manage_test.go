package redfish

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// Volumes is the machine-face RAID management's read model (docs/07-bmc.md
// §6.7): membership resolves through standard Links.Drives first, the
// Huawei Oem.Spans shape as fallback — both spellings must land on the
// same member serials the physical drive table reports.
func TestVolumesMemberResolution(t *testing.T) {
	f := &volumeFixture{
		volumes: map[string]volumeDetail{
			"LogicalDrive0": {Level: "RAID1", DriveIDs: []string{"D0", "D1"}, Capacity: 4000, Standard: true},
			"LogicalDrive1": {Level: "RAID5", DriveIDs: []string{"D0"}, Capacity: 2000},
		},
		order: []string{
			"/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/LogicalDrive0",
			"/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/LogicalDrive1",
		},
	}
	addr := volumeTestAddr(t, f)
	d := New(true, 10*time.Second)

	volumes, err := d.Volumes(context.Background(), addr, bmc.Credentials{Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 {
		t.Fatalf("want 2 volumes, got %d", len(volumes))
	}
	byName := map[string]bmc.VolumeView{}
	for _, v := range volumes {
		byName[v.Name] = v
	}
	// The controller renames (LogicalDriveN) — names are its own Ids.
	ld0 := byName["LogicalDrive0"]
	if ld0.RAIDType != "RAID1" || ld0.SizeBytes != 4000 {
		t.Errorf("LogicalDrive0 = %+v", ld0)
	}
	if len(ld0.MemberSerials) != 2 || ld0.MemberSerials[0] != "SER0" || ld0.MemberSerials[1] != "SER1" {
		t.Errorf("standard Links membership = %v, want [SER0 SER1]", ld0.MemberSerials)
	}
	ld1 := byName["LogicalDrive1"]
	if ld1.RAIDType != "RAID5" {
		t.Errorf("Oem level = %q, want RAID5", ld1.RAIDType)
	}
	if len(ld1.MemberSerials) != 1 || ld1.MemberSerials[0] != "SER0" {
		t.Errorf("Oem Spans membership = %v, want [SER0]", ld1.MemberSerials)
	}
}

// DeleteVolume removes the volume by its controller-assigned name and
// settles the deletion task; unknown names reject without a DELETE.
func TestDeleteVolume(t *testing.T) {
	f := &volumeFixture{
		volumes: map[string]volumeDetail{
			"LogicalDrive0": {Level: "RAID1", DriveIDs: []string{"D0", "D1"}, Capacity: 4000},
		},
		order: []string{"/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/LogicalDrive0"},
	}
	addr := volumeTestAddr(t, f)
	d := New(true, 10*time.Second)
	ctx := context.Background()
	cred := bmc.Credentials{Username: "u", Password: "p"}

	if err := d.DeleteVolume(ctx, addr, cred, "LogicalDrive0"); err != nil {
		t.Fatal(err)
	}
	volumes, err := d.Volumes(ctx, addr, cred)
	if err != nil || len(volumes) != 0 {
		t.Fatalf("volume must be gone after delete: %v, %v", volumes, err)
	}

	if err := d.DeleteVolume(ctx, addr, cred, "Ghost"); err == nil {
		t.Fatal("unknown volume must reject")
	}
}

// The created volume is identified by pre/post diff, not name order: with
// LogicalDrive0..8 already on the controller, the fixture names the new
// volume LogicalDrive10 — which sorts BEFORE LogicalDrive2, so the
// lexicographic "sort and take last" guess hands back LogicalDrive9 (an
// older volume) and would misbind the declarative partition target.
func TestCreateVolumeNamesBeyondNine(t *testing.T) {
	f := &volumeFixture{
		volumes: map[string]volumeDetail{},
		script:  []postReply{{status: 202}},
	}
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("LogicalDrive%d", i)
		// Level RAID0 differs from the requested RAID1 so the idempotency
		// match deliberately misses and the create POST proceeds.
		f.volumes[name] = volumeDetail{Level: "RAID0", DriveIDs: []string{"D0"}, Capacity: 4000}
		f.order = append(f.order, "/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/"+name)
	}
	addr := volumeTestAddr(t, f)
	d := New(true, 10*time.Second)

	name, err := d.CreateVolume(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		bmc.VolumeSpec{Name: "os", RAIDType: "RAID1", MemberSerials: []string{"SER0", "SER1"}})
	if err != nil {
		t.Fatal(err)
	}
	if name != "LogicalDrive10" {
		t.Fatalf("created volume = %q, want LogicalDrive10 (the diff-appearance, not the lexicographic last LogicalDrive9)", name)
	}
}
