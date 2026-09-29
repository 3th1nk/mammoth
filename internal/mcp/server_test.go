package mcp

import (
	"context"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/apiclient"
)

func testServer(t *testing.T, mux http.HandlerFunc) *Server {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{api: apiclient.NewClient(srv.URL, "testtoken")}
}

// ── spec construction ───────────────────────────────────────────────────────

func TestBuildInstallSpecSimplified(t *testing.T) {
	s := &Server{}
	spec, err := s.buildInstallSpec(installSubmitIn{
		MachineIDs:      []string{"mch_1", "mch_2"},
		Distro:          "rocky",
		ImageSource:     "http://mirror/rocky.iso",
		HostnamePattern: "web-{index}",
		SSHKeys:         []string{"ssh-ed25519 AAA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec["boot"].(map[string]any)["strategy"] != "virtual_media" {
		t.Error("default strategy should be virtual_media")
	}
	if spec["image"].(map[string]any)["distro"] != "rocky" {
		t.Error("distro not mapped")
	}
	disk := spec["storage"].(map[string]any)["disks"].([]any)[0].(map[string]any)
	if disk["wipe"] != true {
		t.Error("default template must wipe")
	}
	access := spec["access"].(map[string]any)
	if access["ssh_keys"].([]string)[0] != "ssh-ed25519 AAA" {
		t.Error("ssh keys not mapped")
	}
}

func TestBuildInstallSpecDefaults(t *testing.T) {
	s := &Server{}
	spec, err := s.buildInstallSpec(installSubmitIn{
		MachineIDs:  []string{"mch_1"},
		Distro:      "alpine",
		ImageSource: "file:///cache/alpine.iso",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec["identity"].(map[string]any)["hostname_pattern"] != "node-{index}" {
		t.Error("hostname pattern default missing")
	}
	if keys := spec["access"].(map[string]any)["ssh_keys"].([]string); len(keys) != 0 {
		t.Errorf("ssh_keys should default to empty array, got %v", keys)
	}
}

func TestBuildInstallSpecRawSpecWins(t *testing.T) {
	s := &Server{}
	raw := `{"boot": {"strategy": "pxe"}, "image": {"source": "file:///di.iso", "distro": "alpine"}, "storage": {"disks": []}, "identity": {"hostname_pattern": "pxe-{index}"}}`
	spec, err := s.buildInstallSpec(installSubmitIn{
		MachineIDs:  []string{"mch_1"},
		Distro:      "rocky",
		ImageSource: "http://x/iso",
		RawSpec:     raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec["boot"].(map[string]any)["strategy"] != "pxe" {
		t.Error("raw_spec should override the simplified fields verbatim")
	}
}

func TestBuildInstallSpecRawYAML(t *testing.T) {
	s := &Server{}
	spec, err := s.buildInstallSpec(installSubmitIn{
		MachineIDs: []string{"mch_1"},
		RawSpec: "boot:\n  strategy: pxe\nimage:\n  source: file:///di.iso\n  distro: alpine\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec["boot"].(map[string]any)["strategy"] != "pxe" {
		t.Errorf("YAML raw_spec not honored: %v", spec)
	}
}

func TestBuildInstallSpecWindowsRequiresRawSpec(t *testing.T) {
	s := &Server{}
	if _, err := s.buildInstallSpec(installSubmitIn{
		MachineIDs: []string{"mch_1"}, Distro: "windows-2019", ImageSource: "file:///x.iso",
	}); err == nil {
		t.Fatal("windows without raw_spec must be rejected")
	}
}

func TestBuildInstallSpecRequiredFields(t *testing.T) {
	s := &Server{}
	if _, err := s.buildInstallSpec(installSubmitIn{Distro: "alpine"}); err == nil {
		t.Error("missing machine_ids must error")
	}
	if _, err := s.buildInstallSpec(installSubmitIn{MachineIDs: []string{"mch_1"}}); err == nil {
		t.Error("missing distro/image_source must error")
	}
}

// ── handlers against a mock API ─────────────────────────────────────────────

func TestMachinesListHandler(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/machines" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"items":[{"id":"mch_1","bmc":{"address":"fake://n1"},"state":"ready","power_state":"on"}]}`))
	})
	_, out, err := s.machinesList(context.Background(), nil, machinesListIn{})
	if err != nil {
		t.Fatal(err)
	}
	snap := out.(map[string]any)
	if snap["count"] != 1 {
		t.Fatalf("count = %v", snap["count"])
	}
	row := snap["machines"].([]map[string]any)[0]
	if row["state"] != "ready" || row["id"] != "mch_1" {
		t.Errorf("machine row wrong: %v", row)
	}
}

func TestJobWaitReachesTerminal(t *testing.T) {
	polls := 0
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			w.Write([]byte(`{"items":[{"id":"tsk_1","machine_id":"mch_1","state":"succeeded","stages":[{"name":"install_os","state":"succeeded"},{"name":"verify_ready","state":"succeeded"}]}]}`))
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			w.Write([]byte(`{}`))
		default:
			polls++
			if polls < 2 {
				w.Write([]byte(`{"state":"running"}`))
			} else {
				w.Write([]byte(`{"state":"succeeded"}`))
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, out, err := s.jobWait(ctx, nil, jobWaitIn{JobID: "job_1", TimeoutSecs: 30})
	if err != nil {
		t.Fatal(err)
	}
	snap := out.(map[string]any)
	if snap["state"] != "succeeded" {
		t.Fatalf("final state = %v", snap["state"])
	}
	task := snap["tasks"].([]map[string]any)[0]
	if task["stage"] != "verify_ready" {
		t.Errorf("stage summary wrong: %v", task)
	}
}

func TestJobWaitErrorSurface(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			w.Write([]byte(`{"items":[{"id":"tsk_1","machine_id":"mch_1","state":"failed","stages":[],"error":{"code":"INSTALL_MEDIA_BUILD_FAILED","message":"boot ISO URI is empty"}}]}`))
			return
		}
		w.Write([]byte(`{"state":"failed"}`))
	})
	_, out, err := s.jobWait(context.Background(), nil, jobWaitIn{JobID: "job_9", TimeoutSecs: 5})
	if err != nil {
		t.Fatal(err)
	}
	task := out.(map[string]any)["tasks"].([]map[string]any)[0]
	if !strings.Contains(task["error"].(string), "INSTALL_MEDIA_BUILD_FAILED") {
		t.Errorf("error not surfaced: %v", task)
	}
}

// terminal-state matrix shared with the sandbox watcher's semantics
func TestTerminalJob(t *testing.T) {
	for _, st := range []string{"succeeded", "failed", "canceled", "partial"} {
		if !terminalJob(st) {
			t.Errorf("%s should be terminal", st)
		}
	}
	for _, st := range []string{"pending", "running", "interrupted", ""} {
		if terminalJob(st) {
			t.Errorf("%s should not be terminal", st)
		}
	}
}

