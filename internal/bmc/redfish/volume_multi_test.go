package redfish

// Multi-controller fixtures for the iDRAC8 storage walk: the real R730
// exposes one RAID controller plus two AHCI entries whose Members order is
// unstable across requests (docs/compat/dell.md §2) — the old
// first-controller pick landed on an AHCI entry half the time and reported
// an empty drive pool. These tests pin the order-stable walk.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// multiStorageFixture is a standard-Redfish (Dell-shaped) server: three
// storage controllers whose Members order ROTATES on every request, one of
// them the PERC with Drives + Volumes, the AHCI entries bare.
type multiStorageFixture struct {
	mu        sync.Mutex
	callN     int
	volumeN   int
	volumePST []string // POST bodies seen
}

func (f *multiStorageFixture) order() []string {
	// Deterministic rotation: the RAID controller sits at a different index
	// per call — the shape that broke the first-controller pick.
	switch f.callN % 3 {
	case 0:
		return []string{
			"/redfish/v1/Systems/1/Storage/RAID.Integrated.1-1",
			"/redfish/v1/Systems/1/Storage/AHCI.Embedded.1-1",
			"/redfish/v1/Systems/1/Storage/AHCI.Embedded.2-1",
		}
	case 1:
		return []string{
			"/redfish/v1/Systems/1/Storage/AHCI.Embedded.1-1",
			"/redfish/v1/Systems/1/Storage/RAID.Integrated.1-1",
			"/redfish/v1/Systems/1/Storage/AHCI.Embedded.2-1",
		}
	default:
		return []string{
			"/redfish/v1/Systems/1/Storage/AHCI.Embedded.2-1",
			"/redfish/v1/Systems/1/Storage/AHCI.Embedded.1-1",
			"/redfish/v1/Systems/1/Storage/RAID.Integrated.1-1",
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write fixture: %v", err)
	}
}

func (f *multiStorageFixture) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { writeJSON(t, w, v) }
	members := func(ids []string) map[string]any {
		ms := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			ms = append(ms, map[string]any{"@odata.id": id})
		}
		return map[string]any{"Members": ms, "Members@odata.count": len(ids)}
	}

	mux.HandleFunc("GET /redfish/v1/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"Systems":        map[string]any{"@odata.id": "/redfish/v1/Systems"},
			"SessionService": map[string]any{"@odata.id": "/redfish/v1/SessionService"},
		})
	})
	mux.HandleFunc("POST /redfish/v1/SessionService/Sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Auth-Token", "test-token")
		w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/1")
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{})
	})
	mux.HandleFunc("DELETE /redfish/v1/SessionService/Sessions/1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /redfish/v1/SessionService/Sessions/1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "1"})
	})
	mux.HandleFunc("GET /redfish/v1/Systems", func(w http.ResponseWriter, _ *http.Request) {
		write(w, members([]string{"/redfish/v1/Systems/1"}))
	})
	mux.HandleFunc("GET /redfish/v1/Systems/1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "1", "Storage": map[string]any{"@odata.id": "/redfish/v1/Systems/1/Storage"}})
	})

	// The unstable collection: rotates on every request.
	mux.HandleFunc("GET /redfish/v1/Systems/1/Storage", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.callN++
		order := f.order()
		f.mu.Unlock()
		write(w, members(order))
	})

	// AHCI entries: no Drives, no Volumes.
	for _, ahci := range []string{"AHCI.Embedded.1-1", "AHCI.Embedded.2-1"} {
		p := "/redfish/v1/Systems/1/Storage/" + ahci
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			write(w, map[string]any{"Id": ahci, "@odata.id": p})
		})
	}

	// The PERC: inline Drives (Dell form: @odata.id only) + a Volumes collection.
	raid := "/redfish/v1/Systems/1/Storage/RAID.Integrated.1-1"
	mux.HandleFunc("GET "+raid, func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"Id":        "RAID.Integrated.1-1",
			"@odata.id": raid,
			"Drives": []map[string]any{
				{"@odata.id": raid + "/Drives/Disk.Bay.0"},
				{"@odata.id": raid + "/Drives/Disk.Bay.1"},
			},
			"Drives@odata.count": 2,
			"Volumes":            map[string]any{"@odata.id": raid + "/Volumes"},
		})
	})
	mux.HandleFunc("GET "+raid+"/Drives/Disk.Bay.0", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "Disk.Bay.0", "Model": "ST3000", "SerialNumber": "SERA", "CapacityBytes": 3000})
	})
	mux.HandleFunc("GET "+raid+"/Drives/Disk.Bay.1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "Disk.Bay.1", "Model": "ST3000", "SerialNumber": "SERB", "CapacityBytes": 3000})
	})
	mux.HandleFunc("GET "+raid+"/Volumes", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ids := make([]string, 0, f.volumeN)
		for i := 1; i <= f.volumeN; i++ {
			ids = append(ids, fmt.Sprintf("%s/Volumes/vd%d", raid, i))
		}
		write(w, members(ids))
	})
	mux.HandleFunc("POST "+raid+"/Volumes", func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		f.volumePST = append(f.volumePST, string(raw))
		f.volumeN++
		w.WriteHeader(http.StatusAccepted)
		write(w, map[string]any{"Id": fmt.Sprint(f.volumeN), "TaskState": "Completed"})
	})
	mux.HandleFunc("GET /redfish/v1/TaskService/Tasks/", func(w http.ResponseWriter, req *http.Request) {
		id := strings.TrimPrefix(req.URL.Path, "/redfish/v1/TaskService/Tasks/")
		write(w, map[string]any{"Id": id, "TaskState": "Completed"})
	})
	return mux
}

func multiStorageDriver(t *testing.T, f *multiStorageFixture) (d *Driver, addr string) {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler(t))
	t.Cleanup(srv.Close)
	return New(true, 10*time.Second), strings.TrimPrefix(srv.URL, "https://")
}

// TestPhysicalDrivesAcrossRotatingCollection: the drive pool is found no
// matter where the RAID controller sits in the rotated Members order.
func TestPhysicalDrivesAcrossRotatingCollection(t *testing.T) {
	f := &multiStorageFixture{}
	d, addr := multiStorageDriver(t, f)
	for call := 0; call < 4; call++ { // walk the rotation twice
		drives, err := d.PhysicalDrives(context.Background(), addr, bmc.Credentials{})
		if err != nil {
			t.Fatalf("call %d: PhysicalDrives: %v", call, err)
		}
		if len(drives) != 2 {
			t.Fatalf("call %d: drives = %d, want 2 (the PERC's pool)", call, len(drives))
		}
		serials := map[string]bool{drives[0].Serial: true, drives[1].Serial: true}
		if !serials["SERA"] || !serials["SERB"] {
			t.Errorf("call %d: serials = %v", call, serials)
		}
	}
}

// TestCreateVolumePicksVolumeCapableController: the volume POST lands on the
// controller with the Volumes collection even when it is not the first
// Member.
func TestCreateVolumePicksVolumeCapableController(t *testing.T) {
	f := &multiStorageFixture{}
	d, addr := multiStorageDriver(t, f)
	name, err := d.CreateVolume(context.Background(), addr, bmc.Credentials{}, bmc.VolumeSpec{
		Name:          "vd1",
		RAIDType:      "RAID1",
		MemberSerials: []string{"SERA", "SERB"},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if name == "" {
		t.Fatal("empty volume name")
	}
	if len(f.volumePST) != 1 {
		t.Fatalf("volume POSTs = %d, want 1", len(f.volumePST))
	}
	// The POST must have carried the standard Redfish payload with the
	// RAID controller's drive references.
	if !strings.Contains(f.volumePST[0], `"RAIDType":"RAID1"`) {
		t.Errorf("payload = %s", f.volumePST[0])
	}
}

// TestCreateVolumeWithoutVolumeCapableController: no RAID — the intent is
// rejected with a clear unsupported error instead of a POST to a controller
// that cannot serve it.
func TestCreateVolumeWithoutVolumeCapableController(t *testing.T) {
	f := &multiStorageFixture{}
	d, _ := multiStorageDriver(t, f)

	// Strip the RAID controller: the fixture's Storage collection now only
	// offers the bare AHCI entries.
	mux := http.NewServeMux()
	mux.Handle("/", f.handler(t))
	overlay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/redfish/v1/Systems/1/Storage" {
			writeJSON(t, w, map[string]any{
				"Members": []map[string]any{
					{"@odata.id": "/redfish/v1/Systems/1/Storage/AHCI.Embedded.1-1"},
					{"@odata.id": "/redfish/v1/Systems/1/Storage/AHCI.Embedded.2-1"},
				},
				"Members@odata.count": 2,
			})
			return
		}
		mux.ServeHTTP(w, req)
	}))
	t.Cleanup(overlay.Close)

	_, err := d.CreateVolume(context.Background(), strings.TrimPrefix(overlay.URL, "https://"), bmc.Credentials{}, bmc.VolumeSpec{
		Name:          "vd1",
		RAIDType:      "RAID1",
		MemberSerials: []string{"SERA"},
	})
	if err == nil {
		t.Fatal("CreateVolume without a volume-capable controller should fail")
	}
	e, ok := err.(*bmc.Error)
	if !ok || e.Kind != bmc.KindUnsupported {
		t.Fatalf("error = %v, want KindUnsupported", err)
	}
	if !strings.Contains(e.Detail, "Volumes collection") {
		t.Errorf("detail = %q", e.Detail)
	}
}
