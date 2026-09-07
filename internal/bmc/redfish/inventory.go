package redfish

import (
	"context"
	"strings"

	"github.com/stmcginnis/gofish/redfish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

const gib = 1024 * 1024 * 1024

// CollectInventory gathers the spec-level view (docs/05-inventory.md §2):
// disks/RAID volumes, NICs, CPU, memory, serial. Missing or unenumerable
// resources downgrade coverage to partial rather than failing the probe.
func (d *Driver) CollectInventory(ctx context.Context, addr string, cred bmc.Credentials) (bmc.HardwareView, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return bmc.HardwareView{}, err
	}
	defer c.Logout()

	hw := bmc.HardwareView{Coverage: bmc.CoverageFull}

	systems, err := c.Service.Systems()
	if err != nil || len(systems) == 0 {
		hw.Note("computer system resource not enumerable")
		return hw, nil
	}
	sys := systems[0]
	hw.SerialNumber = sys.SerialNumber

	// CPU: summary first (no per-processor walk needed for the spec view).
	hw.CPU.Model = sys.ProcessorSummary.Model
	hw.CPU.Cores = sys.ProcessorSummary.Count
	if hw.CPU.Model == "" && hw.CPU.Cores == 0 {
		if procs, perr := sys.Processors(); perr == nil && len(procs) > 0 {
			hw.CPU.Model = procs[0].Model
			hw.CPU.Cores = procs[0].TotalCores
		} else {
			hw.Note("processor summary not available")
		}
	}

	if sys.MemorySummary.TotalSystemMemoryGiB > 0 {
		hw.MemoryBytes = int64(sys.MemorySummary.TotalSystemMemoryGiB * gib)
	} else {
		hw.Note("memory summary not available")
	}

	// Disks with the presentation rule: what the OS will see is what the
	// user must see — RAID volumes when the controller reports them,
	// physical drives otherwise (docs/05-inventory.md §2).
	storages, err := sys.Storage()
	if err != nil || len(storages) == 0 {
		hw.Note("storage collection not enumerable")
	} else {
		groups := make([]storageGroup, 0, len(storages))
		for _, st := range storages {
			g := storageGroup{name: st.Name}
			if volumes, verr := st.Volumes(); verr == nil {
				g.volumes = volumes
			}
			if len(g.volumes) == 0 {
				drives, derr := st.Drives()
				g.drives, g.err = drives, derr
			}
			groups = append(groups, g)
		}
		mapStorage(&hw, groups)
	}

	nics, err := sys.EthernetInterfaces()
	if err != nil || len(nics) == 0 {
		hw.Note("ethernet interfaces not enumerable")
	} else {
		hw.NICs = mapNICs(nics)
	}

	if len(hw.Disks) == 0 {
		hw.Note("no drives or volumes reported")
	}
	return hw, nil
}

// storageGroup carries one controller's collected members so the mapping
// stays a pure function (unit-testable against constructed fixtures).
type storageGroup struct {
	name    string
	volumes []*redfish.Volume
	drives  []*redfish.Drive
	err     error // drive enumeration failure for this controller
}

// mapStorage applies the volume-first presentation rule per storage
// controller: a controller that reports volumes presents volumes; a
// controller without volumes presents its physical drives
// (docs/05-inventory.md §2).
func mapStorage(hw *bmc.HardwareView, groups []storageGroup) {
	for _, g := range groups {
		if len(g.volumes) > 0 {
			for _, v := range g.volumes {
				hw.Disks = append(hw.Disks, bmc.DiskView{
					Name:      v.Name,
					SizeBytes: int64(v.CapacityBytes),
					Protocol:  "raid",
					Medium:    "unknown",
				})
			}
			continue
		}
		if g.err != nil {
			hw.Note("storage '" + g.name + "': drives not enumerable")
			continue
		}
		for _, dr := range g.drives {
			hw.Disks = append(hw.Disks, mapDrive(dr))
		}
	}
}

func mapDrive(dr *redfish.Drive) bmc.DiskView {
	protocol := strings.ToLower(string(dr.Protocol))
	medium := "unknown"
	switch dr.MediaType {
	case redfish.HDDMediaType:
		medium = "hdd"
	case redfish.SSDMediaType, redfish.SMRMediaType:
		medium = "ssd"
	}
	name := dr.Name
	if dr.Model != "" && !strings.Contains(name, dr.Model) {
		name = dr.Model + " " + name
	}
	return bmc.DiskView{
		Name:      name,
		Serial:    dr.SerialNumber,
		SizeBytes: dr.CapacityBytes,
		Medium:    medium,
		Protocol:  protocol,
	}
}

func mapNICs(nics []*redfish.EthernetInterface) []bmc.NICView {
	out := make([]bmc.NICView, 0, len(nics))
	for _, n := range nics {
		nic := bmc.NICView{
			Name: n.Name,
			MAC:  n.MACAddress,
		}
		if nic.MAC == "" {
			nic.MAC = n.PermanentMACAddress
		}
		out = append(out, nic)
	}
	return out
}
