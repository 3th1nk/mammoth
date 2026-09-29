package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ── API helper ──────────────────────────────────────────────────────────────
// Duplicated from internal/cli on purpose: the cli package imports sandbox to
// mount the subcommand, so the dependency cannot point the other way.

type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func newAPI(token string) *apiClient {
	return &apiClient{base: apiBase(), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

func (a *apiClient) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var p struct{ Code, Detail string }
		if json.Unmarshal(payload, &p) == nil && p.Code != "" {
			return fmt.Errorf("%s %s: %s (%s)", method, path, p.Detail, p.Code)
		}
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(payload)))
	}
	if out != nil && len(payload) > 0 {
		return json.Unmarshal(payload, out)
	}
	return nil
}

func (a *apiClient) get(path string, out any) error { return a.do(http.MethodGet, path, nil, out) }
func (a *apiClient) post(path string, body, out any) error {
	return a.do(http.MethodPost, path, body, out)
}

// ── install spec ────────────────────────────────────────────────────────────

// buildSpec is field-for-field the verified alpine agent-path shape from
// scripts/agent-dev/e2e.py: one NVMe, ESP/swap/root partitions, DHCP
// networking baked by the agent. No network block — the agent configures
// udhcpc itself.
func buildSpec(isoPath, pubKey string) map[string]any {
	return map[string]any{
		"boot":  map[string]any{"strategy": "virtual_media"},
		"image": map[string]any{"source": "file://" + isoPath, "distro": "alpine"},
		"storage": map[string]any{"disks": []any{map[string]any{
			"select": map[string]any{"match": map[string]any{"type": "nvme", "size": "largest"}},
			"wipe":   true,
			"partitions": []any{
				map[string]any{"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": []string{"esp"}},
				map[string]any{"size": "512M", "fs": "swap"},
				map[string]any{"size": "rest", "fs": "ext4", "mount": "/"},
			},
		}}},
		"identity": map[string]any{"hostname_pattern": "sandbox-node-{index}"},
		"access":   map[string]any{"ssh_keys": []string{pubKey}},
	}
}

// bootToken pulls the per-task render token out of a task answer_url
// ("…/render/<token>/<file>"); the boot ISO lands at
// <media>/boot/boot-<token>.iso (internal/provision/strategy_virtualmedia.go).
func bootToken(answerURL string) string {
	_, rest, ok := strings.Cut(answerURL, "/render/")
	if !ok {
		return ""
	}
	tok, _, _ := strings.Cut(rest, "/")
	return tok
}

func bootISOPath(mediaDir, token string) string {
	return filepath.Join(mediaDir, "boot", "boot-"+token+".iso")
}

// ── task view ───────────────────────────────────────────────────────────────

type taskView struct {
	MachineID string `json:"machine_id"`
	State     string `json:"state"`
	AnswerURL string `json:"answer_url"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Stages []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"stages"`
}

func terminalJob(state string) bool {
	switch state {
	case "succeeded", "failed", "canceled", "partial":
		return true
	}
	return false
}

func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}

// ── up ──────────────────────────────────────────────────────────────────────

type UpOptions struct {
	Nodes int
	Keep  bool // on failure/interrupt, leave processes and state for debugging
}

// Up runs the whole sandbox flow. On success everything keeps running and the
// operator is left with ssh instructions; on failure (or Ctrl-C) the sandbox
// is torn down unless Keep was set.
func Up(ctx context.Context, opts UpOptions) error {
	if opts.Nodes < 1 || opts.Nodes > MaxNodes {
		return fmt.Errorf("--nodes must be between 1 and %d", MaxNodes)
	}
	if old, err := LoadState(); err == nil && stateAlive(old) {
		return fmt.Errorf("a sandbox is already up (server pid %d) — inspect with `mammoth sandbox status`, stop with `mammoth sandbox down`", old.ServerPID)
	}

	dir := Dir()
	for _, sub := range []string{"media", "logs", "disks"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the mammoth binary: %w", err)
	}

	state := &State{
		Version: 1, Nodes: opts.Nodes, Port: httpPort(), DBPort: dbPort(),
		Container: container, Token: mustSecret(), CreatedAt: time.Now().UTC(),
	}
	save := func() {
		if err := state.Save(); err != nil {
			fmt.Printf("  (warning: state save failed: %v)\n", err)
		}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = runUp(ctx, exe, state, save)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("interrupted: %w", err)
	}
	if opts.Keep {
		fmt.Printf("  sandbox state kept for debugging (%s) — tear down with `mammoth sandbox down`\n", dir)
	} else {
		fmt.Println("· tearing down")
		teardownState(state)
		fmt.Println("  removed")
	}
	return err
}

func runUp(ctx context.Context, exe string, state *State, save func()) error {
	dir := Dir()
	logs := filepath.Join(dir, "logs")

	fmt.Println("· checking host dependencies")
	if err := CheckDependencies(state.Nodes); err != nil {
		return err
	}

	fmt.Printf("· fetching the alpine extended ISO (%s)\n", alpineVer)
	isoPath, err := EnsureISO(dir)
	if err != nil {
		return err
	}

	fmt.Printf("· starting postgres (%s container)\n", container)
	reused, err := ensurePostgres()
	if err != nil {
		return err
	}
	if reused {
		fmt.Println("  reusing the running container")
	}
	save()
	if err := waitDB(ctx); err != nil {
		return err
	}

	fmt.Printf("· starting mammoth (%s)\n", apiBase())
	serverPID, err := startServer(exe, state.Token, dir, filepath.Join(logs, "server.log"))
	if err != nil {
		return err
	}
	state.ServerPID = serverPID
	save()
	if err := waitHealthy(ctx); err != nil {
		fmt.Println("  last server log lines:")
		for _, l := range tailFile(filepath.Join(logs, "server.log"), 30) {
			fmt.Println("    " + l)
		}
		return err
	}
	api := newAPI(state.Token)

	fmt.Printf("· registering %d sandbox machines (fake BMC)\n", state.Nodes)
	var cred struct {
		ID string `json:"id"`
	}
	if err := api.post("/api/v1/credentials", map[string]any{
		"type": "bmc", "name": "sandbox",
		"secret": map[string]string{"username": "sandbox", "password": "sandbox"},
	}, &cred); err != nil {
		return err
	}
	state.CredentialID = cred.ID
	save()
	stamp := time.Now().Unix()
	for i := 0; i < state.Nodes; i++ {
		var m struct {
			ID string `json:"id"`
		}
		if err := api.post("/api/v1/machines", map[string]any{
			"bmc": map[string]any{
				"address":       fmt.Sprintf("fake://sandbox-node-%d-%d", i+1, stamp),
				"protocol":      "fake",
				"credential_id": state.CredentialID,
			},
		}, &m); err != nil {
			return err
		}
		state.MachineIDs = append(state.MachineIDs, m.ID)
		save()
	}
	for i, mid := range state.MachineIDs {
		if err := waitReady(ctx, api, mid); err != nil {
			return err
		}
		fmt.Printf("  ✓ sandbox-node-%d discovered (machine %s)\n", i+1, mid)
	}

	pubKey, err := ensureSSHKey(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		return err
	}

	fmt.Printf("· submitting the alpine install job (%d machines, virtual_media)\n", state.Nodes)
	var job struct {
		ID string `json:"id"`
	}
	if err := api.post("/api/v1/jobs", map[string]any{
		"type":    "install",
		"targets": map[string]any{"machine_ids": state.MachineIDs},
		"spec":    buildSpec(isoPath, pubKey),
	}, &job); err != nil {
		return err
	}
	state.JobID = job.ID
	save()

	if err := bootGuests(ctx, api, state, save); err != nil {
		return err
	}

	fmt.Printf("· install running — watching job %s (up to %s without KVM)\n", state.JobID, installWait)
	if err := watchInstall(ctx, api, state); err != nil {
		return err
	}

	return bootInstalled(ctx, state, save)
}

// bootGuests polls the job's tasks; as soon as a task's boot ISO is rendered
// (<media>/boot-<token>.iso) its qemu guest starts. Returns when every node
// has a guest in the installer.
func bootGuests(ctx context.Context, api *apiClient, state *State, save func()) error {
	mediaDir := filepath.Join(Dir(), "media")
	booted := make([]bool, state.Nodes)
	deadline := time.Now().Add(prepareMedia)
	for {
		var tasks struct {
			Items []taskView `json:"items"`
		}
		if err := api.get(fmt.Sprintf("/api/v1/jobs/%s/tasks?page_size=200", state.JobID), &tasks); err != nil {
			return err
		}
		for _, t := range tasks.Items {
			idx := indexOf(state.MachineIDs, t.MachineID)
			if idx < 0 || booted[idx] || t.AnswerURL == "" {
				continue
			}
			tok := bootToken(t.AnswerURL)
			if tok == "" {
				continue
			}
			iso := bootISOPath(mediaDir, tok)
			if _, err := os.Stat(iso); err != nil {
				continue
			}
			disk := filepath.Join(Dir(), "disks", fmt.Sprintf("node%d.raw", idx+1))
			_ = os.Remove(disk) // a leftover disk from an earlier run must not leak in
			if out, err := exec.Command("qemu-img", "create", "-f", "raw", disk,
				fmt.Sprintf("%dG", diskGB)).CombinedOutput(); err != nil {
				return fmt.Errorf("qemu-img create: %s", firstLine(out))
			}
			serial := filepath.Join(Dir(), "logs", fmt.Sprintf("node%d-serial.log", idx+1))
			cmd, closeLog, err := qemuInstallCmd(iso, disk, serial)
			if err != nil {
				return err
			}
			pid, err := startDetached(cmd)
			closeLog()
			if err != nil {
				return fmt.Errorf("start qemu (node %d): %w", idx+1, err)
			}
			state.InstallPIDs = append(state.InstallPIDs, pid)
			save()
			booted[idx] = true
			fmt.Printf("  ✓ sandbox-node-%d booting the installer (qemu pid %d, boot token %s)\n", idx+1, pid, tok)
		}
		all := true
		for _, b := range booted {
			all = all && b
		}
		if all {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("boot media never appeared within %s", prepareMedia)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func waitReady(ctx context.Context, api *apiClient, id string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		var m struct {
			State string `json:"state"`
		}
		if err := api.get("/api/v1/machines/"+id, &m); err == nil && m.State == "ready" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("machine %s not ready within 60s", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func watchInstall(ctx context.Context, api *apiClient, state *State) error {
	deadline := time.Now().Add(installWait)
	prev := ""
	for {
		var job struct {
			State string `json:"state"`
		}
		if err := api.get("/api/v1/jobs/"+state.JobID, &job); err != nil {
			return err
		}
		var tasks struct {
			Items []taskView `json:"items"`
		}
		if err := api.get(fmt.Sprintf("/api/v1/jobs/%s/tasks?page_size=200", state.JobID), &tasks); err != nil {
			return err
		}
		if line := progressLine(state, tasks.Items); line != prev {
			fmt.Println(line)
			prev = line
		}
		if terminalJob(job.State) {
			if job.State != "succeeded" {
				dumpFailure(state, tasks.Items)
				return fmt.Errorf("install job ended in state %s", job.State)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the install is still running after %s (serial logs: %s/logs/node*-serial.log)", installWait, Dir())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func progressLine(state *State, tasks []taskView) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		idx := indexOf(state.MachineIDs, t.MachineID) + 1
		last := ""
		for _, s := range t.Stages {
			if s.State != "pending" {
				last = s.Name
			}
		}
		st := t.State
		if last != "" {
			st = fmt.Sprintf("%s@%s", t.State, last)
		}
		parts = append(parts, fmt.Sprintf("node%d %s", idx, st))
	}
	return "  " + strings.Join(parts, " | ")
}

func dumpFailure(state *State, tasks []taskView) {
	for _, t := range tasks {
		if t.State == "succeeded" || t.Error == nil {
			continue
		}
		fmt.Printf("  ✗ node task failed: %s: %s\n", t.Error.Code, t.Error.Message)
	}
	for i := 1; i <= state.Nodes; i++ {
		serial := filepath.Join(Dir(), "logs", fmt.Sprintf("node%d-serial.log", i))
		if lines := tailFile(serial, 15); len(lines) > 0 {
			fmt.Printf("  ── node%d serial tail ──\n", i)
			for _, l := range lines {
				fmt.Println("    " + l)
			}
		}
	}
}

// bootInstalled second-boots every installed disk with ssh forwarded, then
// probes each node's sshd for the hostname.
func bootInstalled(ctx context.Context, state *State, save func()) error {
	dir := Dir()
	keyPath := filepath.Join(dir, "id_ed25519")

	fmt.Println("· booting the installed systems (ssh forwarded to localhost)")
	for i := 0; i < state.Nodes; i++ {
		serial := filepath.Join(dir, "logs", fmt.Sprintf("node%d-boot.log", i+1))
		disk := filepath.Join(dir, "disks", fmt.Sprintf("node%d.raw", i+1))
		cmd, closeLog, err := qemuBootCmd(disk, serial, sshBase()+i)
		if err != nil {
			return err
		}
		pid, err := startDetached(cmd)
		closeLog()
		if err != nil {
			return fmt.Errorf("boot qemu (node %d): %w", i+1, err)
		}
		state.BootPIDs = append(state.BootPIDs, pid)
		save()
	}
	fmt.Printf("· waiting for sshd on the installed systems (up to %s per node)\n", bootWait)
	for i := 0; i < state.Nodes; i++ {
		if host := sshProbe(ctx, keyPath, sshBase()+i, bootWait); host == "" {
			fmt.Printf("  ⚠ sandbox-node-%d: ssh did not answer on port %d yet — retry manually\n", i+1, sshBase()+i)
		} else {
			fmt.Printf("  ✓ sandbox-node-%d is up (hostname %q)\n", i+1, host)
		}
	}

	fmt.Printf("\nSandbox is up — %d alpine machines installed end to end.\n", state.Nodes)
	for i := 0; i < state.Nodes; i++ {
		fmt.Printf("  ssh -i %s -p %d root@127.0.0.1   # sandbox-node-%d\n", keyPath, sshBase()+i, i+1)
	}
	fmt.Println("Inspect: mammoth sandbox status · Tear down: mammoth sandbox down")
	return nil
}

// sshProbe waits for the node's sshd and returns the reported hostname.
func sshProbe(ctx context.Context, keyPath string, port int, wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		out, err := exec.Command("ssh", "-i", keyPath,
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5", "-o", "BatchMode=yes",
			"-p", fmt.Sprint(port), "root@127.0.0.1", "hostname").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(5 * time.Second):
		}
	}
}

// ensureSSHKey creates the sandbox's one-shot keypair (idempotent).
func ensureSSHKey(path string) (string, error) {
	pubPath := path + ".pub"
	if b, err := os.ReadFile(pubPath); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	_ = os.Remove(path) // a lone private key would make ssh-keygen prompt
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen: %s", firstLine(out))
	}
	b, err := os.ReadFile(pubPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
