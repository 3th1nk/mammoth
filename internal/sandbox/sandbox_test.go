package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MAMMOTH_SANDBOX_DIR", dir)
	return dir
}

func TestStateRoundTrip(t *testing.T) {
	dir := testDir(t)
	st := &State{
		Version: 1, Nodes: 3, Port: httpPort(), DBPort: dbPort(),
		Container: container, Token: "tok", ServerPID: 4242,
		InstallPIDs: []int{11, 12}, BootPIDs: []int{21},
		CredentialID: "cred-1", MachineIDs: []string{"m1", "m2", "m3"},
		JobID: "job-9",
	}
	if err := st.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Nodes != 3 || got.Token != "tok" || got.ServerPID != 4242 ||
		len(got.MachineIDs) != 3 || got.BootPIDs[0] != 21 || got.JobID != "job-9" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if filepath.Dir(statePath()) != dir {
		t.Fatalf("state path %s not under %s", statePath(), dir)
	}
	removeState()
	if _, err := LoadState(); err == nil {
		t.Fatal("state should be gone after removeState")
	}
}

func TestBuildSpecMatchesVerifiedShape(t *testing.T) {
	spec := buildSpec("/cache/alpine.iso", "ssh-ed25519 AAA key")
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["boot"].(map[string]any)["strategy"] != "virtual_media" {
		t.Fatal("boot strategy must be virtual_media")
	}
	img := m["image"].(map[string]any)
	if img["distro"] != "alpine" || img["source"] != "file:///cache/alpine.iso" {
		t.Fatalf("image block wrong: %v", img)
	}
	disk := m["storage"].(map[string]any)["disks"].([]any)[0].(map[string]any)
	sel := disk["select"].(map[string]any)["match"].(map[string]any)
	if sel["type"] != "nvme" || sel["size"] != "largest" || disk["wipe"] != true {
		t.Fatalf("disk select wrong: %v", disk)
	}
	parts := disk["partitions"].([]any)
	if len(parts) != 3 {
		t.Fatalf("want 3 partitions, got %d", len(parts))
	}
	root := parts[2].(map[string]any)
	if root["size"] != "rest" || root["fs"] != "ext4" {
		t.Fatalf("root partition wrong: %v", root)
	}
	if m["identity"].(map[string]any)["hostname_pattern"] != "sandbox-node-{index}" {
		t.Fatal("hostname pattern wrong")
	}
	keys := m["access"].(map[string]any)["ssh_keys"].([]any)
	if len(keys) != 1 || keys[0] != "ssh-ed25519 AAA key" {
		t.Fatal("ssh key not baked into access")
	}
	// the verified agent-path spec carries no network block; the agent
	// configures DHCP itself
	if _, ok := m["network"]; ok {
		t.Fatal("spec must not carry a network block")
	}
}

func TestBootToken(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:18080/render/abc123/agent-plan.sh": "abc123",
		"http://127.0.0.1:18080/render/tok/probe-report":     "tok",
		"http://host/render/onlytoken":                       "onlytoken",
		"http://host/no-render-here":                         "",
		"":                                                   "",
	}
	for in, want := range cases {
		if got := bootToken(in); got != want {
			t.Errorf("bootToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCachedChecksum(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "sum")
	content := "58b0a2f3e39b0b1d1e0d9f8dcd1a5d4cb1b3a0f7c7f8f9c30e4d8b7a6c5d4e3f  " + alpineISO + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := cachedChecksum(path)
	if err != nil {
		t.Fatalf("cachedChecksum: %v", err)
	}
	if got != "58b0a2f3e39b0b1d1e0d9f8dcd1a5d4cb1b3a0f7c7f8f9c30e4d8b7a6c5d4e3f" {
		t.Fatalf("wrong hash parsed: %s", got)
	}
	if err := os.WriteFile(path, []byte("deadbeef  some-other.iso\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedChecksum(path); err == nil {
		t.Fatal("expected an error when the checksum file lacks our ISO")
	}
}

func TestRequiredPortsScalesWithNodes(t *testing.T) {
	ports := requiredPorts(3)
	if len(ports) != 5 || ports[0] != httpPort() || ports[1] != dbPort() {
		t.Fatalf("unexpected ports: %v", ports)
	}
	if ports[2] != sshBase() || ports[4] != sshBase()+2 {
		t.Fatalf("ssh forwards wrong: %v", ports)
	}
}

func TestFileChecksumStable(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, b := fileChecksum(p), fileChecksum(p)
	if a == "" || a != b {
		t.Fatalf("checksum unstable or empty: %q vs %q", a, b)
	}
}
