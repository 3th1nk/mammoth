package redfish

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

// volumeFixture emulates the slice of a Huawei-style controller the volume
// capability talks to (docs/compat/huawei.md): inline drive IDs on the
// storage resource, OEM-flavored volume creation, 202 tasks.
type volumeFixture struct {
	mu sync.Mutex

	volumes map[string]volumeDetail // name → detail (span members = drive Ids)
	order   []string

	posts  []string // captured Volumes POST bodies
	taskN  int
	taskSt map[string]string
	script []postReply // responses for consecutive Volumes POSTs; last repeats
}

type volumeDetail struct {
	Level    string
	DriveIDs []string
	Capacity int64
}

type postReply struct {
	status int
	body   string
}

func (f *volumeFixture) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	members := func(ids []string) map[string]any {
		ms := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			ms = append(ms, map[string]any{"@odata.id": id})
		}
		return map[string]any{"Members": ms, "Members@odata.count": len(ms)}
	}

	mux.HandleFunc("GET /redfish/v1/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"RedfishVersion": "1.9.0", "Id": "RootService",
			"Oem":            map[string]any{"Huawei": map[string]any{"Domain": "iBMC"}},
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
	mux.HandleFunc("GET /redfish/v1/SessionService/Sessions/1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "1"})
	})
	mux.HandleFunc("DELETE /redfish/v1/SessionService/Sessions/1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /redfish/v1/Systems", func(w http.ResponseWriter, _ *http.Request) {
		write(w, members([]string{"/redfish/v1/Systems/1"}))
	})
	mux.HandleFunc("GET /redfish/v1/Systems/1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "1", "Storage": map[string]any{"@odata.id": "/redfish/v1/Systems/1/Storages"}})
	})
	mux.HandleFunc("GET /redfish/v1/Systems/1/Storages", func(w http.ResponseWriter, _ *http.Request) {
		write(w, members([]string{"/redfish/v1/Systems/1/Storages/RAIDStorage0"}))
	})
	mux.HandleFunc("GET /redfish/v1/Systems/1/Storages/RAIDStorage0", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"Id":        "RAIDStorage0",
			"@odata.id": "/redfish/v1/Systems/1/Storages/RAIDStorage0",
			"Drives": []map[string]any{
				{"@odata.id": "/redfish/v1/Chassis/1/Drives/D0", "Oem": map[string]any{"Huawei": map[string]any{"DriveID": 0}}},
				{"@odata.id": "/redfish/v1/Chassis/1/Drives/D1", "Oem": map[string]any{"Huawei": map[string]any{"DriveID": 1}}},
			},
			"Volumes": map[string]any{"@odata.id": "/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes"},
		})
	})
	mux.HandleFunc("GET /redfish/v1/Chassis/1/Drives/D0", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "D0", "Model": "ST4000", "SerialNumber": "SER0", "CapacityBytes": 4000})
	})
	mux.HandleFunc("GET /redfish/v1/Chassis/1/Drives/D1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Id": "D1", "Model": "ST4000", "SerialNumber": "SER1", "CapacityBytes": 4000})
	})
	mux.HandleFunc("GET /redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write(w, members(f.order))
	})
	mux.HandleFunc("GET /redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/", func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := strings.TrimPrefix(req.URL.Path, "/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/")
		v, ok := f.volumes[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		drives := make([]map[string]any, 0, len(v.DriveIDs))
		for _, d := range v.DriveIDs {
			drives = append(drives, map[string]any{"@odata.id": "/redfish/v1/Chassis/1/Drives/" + d})
		}
		write(w, map[string]any{"Id": name, "@odata.id": "/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/" + name, "RAIDType": "None",
			"Oem": map[string]any{"Huawei": map[string]any{
				"VolumeRaidLevel": v.Level,
				"Spans":           []map[string]any{{"Drives": drives}},
			}}})
	})
	mux.HandleFunc("POST /redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes", func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		f.posts = append(f.posts, string(raw))

		reply := f.script[0]
		if len(f.script) > 1 {
			f.script = f.script[1:]
		}
		if reply.status != http.StatusAccepted {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.status)
			_, _ = w.Write([]byte(reply.body))
			return
		}
		// Accepted: realize the created volume from the payload, 202 + task.
		if f.volumes == nil {
			f.volumes = map[string]volumeDetail{}
		}
		f.taskN++
		tid := fmt.Sprint(f.taskN)
		if f.taskSt == nil {
			f.taskSt = map[string]string{}
		}
		f.taskSt[tid] = "Completed"
		name := fmt.Sprintf("LogicalDrive%d", len(f.order)+1)
		level, driveIDs := "RAID0", []string{"D0"}
		if oem, ok := body["Oem"].(map[string]any); ok {
			if hw, ok := oem["Huawei"].(map[string]any); ok {
				if l, ok := hw["VolumeRaidLevel"].(string); ok {
					level = l
				}
				if ids, ok := hw["Drives"].([]any); ok {
					driveIDs = nil
					for _, i := range ids {
						driveIDs = append(driveIDs, fmt.Sprintf("D%d", int(i.(float64))))
					}
				}
			}
		} else if l, ok := body["RAIDType"].(string); ok {
			level = l
		}
		f.volumes[name] = volumeDetail{Level: level, DriveIDs: driveIDs, Capacity: 4000}
		f.order = append(f.order, "/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/"+name)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"Id":"` + tid + `","TaskState":"Running"}`))
	})
	mux.HandleFunc("GET /redfish/v1/TaskService/Tasks/", func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := strings.TrimPrefix(req.URL.Path, "/redfish/v1/TaskService/Tasks/")
		write(w, map[string]any{"Id": id, "TaskState": f.taskSt[id]})
	})
	return mux
}

func volumeTestDriver(t *testing.T, f *volumeFixture) *Driver {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	return New(true, 10*time.Second)
}

func volumeTestAddr(t *testing.T, f *volumeFixture) string {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

var _ = context.Background
var _ = bmc.Credentials{}
var _ = time.Second

// Idempotency is by member-serial set + level: a volume already spanning
// exactly the requested drives is returned as-is, no POST issued — pipeline
// retries and re-runs must not rebuild arrays.
func TestCreateVolumeIdempotentByMembers(t *testing.T) {
	f := &volumeFixture{
		volumes: map[string]volumeDetail{
			"LogicalDrive0": {Level: "RAID1", DriveIDs: []string{"D0", "D1"}, Capacity: 4000},
		},
		order: []string{"/redfish/v1/Systems/1/Storages/RAIDStorage0/Volumes/LogicalDrive0"},
	}
	addr := volumeTestAddr(t, f)
	d := New(true, 10*time.Second)

	name, err := d.CreateVolume(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		bmc.VolumeSpec{Name: "mammoth-ld", RAIDType: "RAID1", MemberSerials: []string{"SER0", "SER1"}})
	if err != nil {
		t.Fatal(err)
	}
	if name != "LogicalDrive0" {
		t.Fatalf("want existing volume name, got %q", name)
	}
	if len(f.posts) != 0 {
		t.Fatalf("idempotent hit must not POST, got %v", f.posts)
	}
}

// The controller rejects the standard payload (PropertyUnknown RAIDType) —
// the driver must fall back to the OEM flavor with integer DriveIDs
// (docs/compat/huawei.md), poll the task, and bind via the name the
// controller assigned.
func TestCreateVolumeHuaweiFallback(t *testing.T) {
	f := &volumeFixture{
		script: []postReply{
			{status: http.StatusBadRequest, body: `{"error":{"code":"Base.1.0.GeneralError","@Message.ExtendedInfo":[{"MessageId":"Base.1.0.PropertyUnknown","RelatedProperties":["#/RAIDType"]}]}}`},
			{status: http.StatusAccepted},
		},
	}
	addr := volumeTestAddr(t, f)
	d := New(true, 10*time.Second)

	name, err := d.CreateVolume(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		bmc.VolumeSpec{Name: "mammoth-ld", RAIDType: "RAID1", MemberSerials: []string{"SER0", "SER1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 2 {
		t.Fatalf("want generic POST then OEM POST, got %d posts", len(f.posts))
	}
	if !strings.Contains(f.posts[0], `"RAIDType":"RAID1"`) {
		t.Errorf("generic payload missing standard RAIDType: %s", f.posts[0])
	}
	if !strings.Contains(f.posts[1], `"VolumeRaidLevel":"RAID1"`) ||
		!strings.Contains(f.posts[1], `"Drives":[0,1]`) {
		t.Errorf("OEM payload wrong: %s", f.posts[1])
	}
	if name != "LogicalDrive1" {
		t.Fatalf("want controller-assigned name of the new volume, got %q", name)
	}
}

// A controller accepting the standard payload needs no vendor fallback —
// the generic-first path is what keeps the capability brand-neutral.
func TestCreateVolumeStandardPath(t *testing.T) {
	f := &volumeFixture{
		script: []postReply{{status: http.StatusAccepted}},
	}
	addr := volumeTestAddr(t, f)
	d := New(true, 10*time.Second)

	name, err := d.CreateVolume(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		bmc.VolumeSpec{Name: "vol0", RAIDType: "RAID0", MemberSerials: []string{"SER0"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || !strings.Contains(f.posts[0], `"RAIDType":"RAID0"`) {
		t.Fatalf("want exactly one standard POST, got %v", f.posts)
	}
	if name != "LogicalDrive1" {
		t.Fatalf("want new volume name, got %q", name)
	}
}
