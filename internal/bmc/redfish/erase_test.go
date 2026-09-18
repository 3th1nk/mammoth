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

// eraseFixture emulates the controller slice the secure-erase capability
// talks to: storage resource with inline drive links, drive resources with
// the #Drive.SecureErase action, 202 tasks. Drives listed in noEraseAction
// omit the action — the unsupported-purge case.
type eraseFixture struct {
	mu sync.Mutex

	noEraseAction map[string]bool // drive Id → no action declared
	erasePosts    []string        // captured action target POSTs
	eraseStatus   int             // POST reply status; default 204
	taskN         int
	lastTask      string
}

func (f *eraseFixture) handler() http.Handler {
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
		return map[string]any{"Members": ms}
	}

	mux.HandleFunc("GET /redfish/v1/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"RedfishVersion": "1.9.0", "Id": "RootService",
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
				{"@odata.id": "/redfish/v1/Chassis/1/Drives/D0"},
				{"@odata.id": "/redfish/v1/Chassis/1/Drives/D1"},
				{"@odata.id": "/redfish/v1/Chassis/1/Drives/D2"},
			},
		})
	})
	for _, d := range []string{"D0", "D1", "D2"} {
		d := d
		mux.HandleFunc("GET /redfish/v1/Chassis/1/Drives/"+d, func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			plain := f.noEraseAction[d]
			f.mu.Unlock()
			body := map[string]any{
				"Id": d, "Model": "ST4000",
				"SerialNumber":  "SER" + d[1:],
				"CapacityBytes": 4000,
			}
			if !plain {
				body["Actions"] = map[string]any{
					"#Drive.SecureErase": map[string]any{
						"target": "/redfish/v1/Chassis/1/Drives/" + d + "/Actions/Drive.SecureErase",
					},
				}
			}
			write(w, body)
		})
	}
	mux.HandleFunc("POST /", func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/Actions/Drive.SecureErase") {
			http.NotFound(w, req)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.erasePosts = append(f.erasePosts, req.URL.Path)
		status := f.eraseStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		if status == http.StatusAccepted {
			f.taskN++
			tid := fmt.Sprint(f.taskN)
			f.lastTask = tid
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"Id":"` + tid + `","TaskState":"Running"}`))
			return
		}
		w.WriteHeader(status)
	})
	mux.HandleFunc("GET /redfish/v1/TaskService/Tasks/", func(w http.ResponseWriter, req *http.Request) {
		id := strings.TrimPrefix(req.URL.Path, "/redfish/v1/TaskService/Tasks/")
		write(w, map[string]any{"Id": id, "TaskState": "Completed"})
	})
	return mux
}

func eraseTestAddr(t *testing.T, f *eraseFixture) string {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

// The happy path: both drives erased via their action targets, in request
// order, tasks polled where the controller answers 202.
func TestSecureEraseHappyPath(t *testing.T) {
	f := &eraseFixture{}
	addr := eraseTestAddr(t, f)
	d := New(true, 10*time.Second)

	results, err := d.SecureErase(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		[]string{"SER0", "SER1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Serial != "SER0" || results[1].Serial != "SER1" {
		t.Fatalf("results wrong: %+v", results)
	}
	want := []string{
		"/redfish/v1/Chassis/1/Drives/D0/Actions/Drive.SecureErase",
		"/redfish/v1/Chassis/1/Drives/D1/Actions/Drive.SecureErase",
	}
	if strings.Join(f.erasePosts, "|") != strings.Join(want, "|") {
		t.Fatalf("POSTs wrong: %v", f.erasePosts)
	}
}

// One unknown serial aborts the whole request BEFORE anything is touched:
// zero POSTs, protocol error naming the serial.
func TestSecureEraseUnknownSerialTouchesNothing(t *testing.T) {
	f := &eraseFixture{}
	addr := eraseTestAddr(t, f)
	d := New(true, 10*time.Second)

	_, err := d.SecureErase(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		[]string{"SER0", "GHOST"})
	var bmcErr *bmc.Error
	if err == nil || !asBMCError(err, &bmcErr) || bmcErr.Kind != bmc.KindProtocolError {
		t.Fatalf("want protocol error, got %v", err)
	}
	if !strings.Contains(err.Error(), "GHOST") {
		t.Fatalf("error must name the serial: %v", err)
	}
	if len(f.erasePosts) != 0 {
		t.Fatalf("nothing may be erased when a serial does not resolve: %v", f.erasePosts)
	}
}

// A drive without the action is a hard unsupported — the controller cannot
// purge it and the caller must know (NIST 800-88 demands a method).
func TestSecureEraseUnsupportedDrive(t *testing.T) {
	f := &eraseFixture{noEraseAction: map[string]bool{"D1": true}}
	addr := eraseTestAddr(t, f)
	d := New(true, 10*time.Second)

	_, err := d.SecureErase(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		[]string{"SER1"})
	var bmcErr *bmc.Error
	if err == nil || !asBMCError(err, &bmcErr) || bmcErr.Kind != bmc.KindUnsupported {
		t.Fatalf("want unsupported, got %v", err)
	}
	if len(f.erasePosts) != 0 {
		t.Fatalf("no POST expected for a drive without the action: %v", f.erasePosts)
	}
}

// Empty request is rejected locally.
func TestSecureEraseEmptyRequest(t *testing.T) {
	d := New(true, 10*time.Second)
	if _, err := d.SecureErase(context.Background(), "unused",
		bmc.Credentials{}, nil); err == nil {
		t.Fatal("empty serials must be rejected")
	}
}

// The 202 shape: the controller accepts with a task reference; the driver
// polls it to Completed and reports success.
func TestSecureEraseTaskPoll(t *testing.T) {
	f := &eraseFixture{eraseStatus: http.StatusAccepted}
	addr := eraseTestAddr(t, f)
	d := New(true, 10*time.Second)

	results, err := d.SecureErase(context.Background(), addr,
		bmc.Credentials{Username: "u", Password: "p"},
		[]string{"SER0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Serial != "SER0" {
		t.Fatalf("results wrong: %+v", results)
	}
	if f.lastTask == "" {
		t.Fatal("202 must carry a task the driver polls")
	}
}

func asBMCError(err error, target **bmc.Error) bool {
	if e, ok := err.(*bmc.Error); ok {
		*target = e
		return true
	}
	return false
}
