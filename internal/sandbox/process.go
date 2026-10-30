package sandbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// State is everything `sandbox status` / `down` need to re-find a running
// sandbox after the `up` process exited: pids, container, ports, API token.
type State struct {
	Version      int       `json:"version"`
	Nodes        int       `json:"nodes"`
	Port         int       `json:"port"`
	DBPort       int       `json:"db_port"`
	Token        string    `json:"token"`
	Container    string    `json:"container"`
	ServerPID    int       `json:"server_pid"`
	InstallPIDs  []int     `json:"install_pids"`
	BootPIDs     []int     `json:"boot_pids"`
	CredentialID string    `json:"credential_id"`
	MachineIDs   []string  `json:"machine_ids"`
	JobID        string    `json:"job_id"`
	CreatedAt    time.Time `json:"created_at"`
}

func statePath() string { return filepath.Join(Dir(), "state.json") }

func LoadState() (*State, error) {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", statePath(), err)
	}
	return &st, nil
}

func (st *State) Save() error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), append(b, '\n'), 0o600)
}

func removeState() { _ = os.Remove(statePath()) }

// Up checks whether a previous sandbox is still running: state.json exists and
// its server pid (or container) is alive.
func stateAlive(st *State) bool {
	return st != nil && (pidAlive(st.ServerPID) || containerRunning(st.Container))
}

// ── host processes ──────────────────────────────────────────────────────────

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// startDetached launches a long-lived child in its own process group (so the
// interactive `sandbox up` shell can exit without SIGHUP-ing it) and reaps it
// in the background to avoid zombies.
func startDetached(cmd *exec.Cmd) (int, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

func terminate(pid int) {
	if !pidAlive(pid) {
		return
	}
	p, _ := os.FindProcess(pid)
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && pidAlive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = p.Signal(syscall.SIGKILL)
	}
}

// envWith replaces the environment for child processes: inherited vars minus
// every MAMMOTH_* (the operator's shell may export some), then our overrides.
func envWith(overrides map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MAMMOTH_") {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// ── postgres (the only docker footprint) ────────────────────────────────────

func containerRunning(name string) bool {
	out, err := exec.Command("docker", "ps", "--filter", "name="+name,
		"--filter", "status=running", "--format", "{{.Names}}").Output()
	return err == nil && strings.TrimSpace(string(out)) == name
}

// ensurePostgres starts (or reuses) the sandbox postgres container.
func ensurePostgres() (reused bool, err error) {
	if containerRunning(container) {
		return true, nil
	}
	_ = exec.Command("docker", "rm", "-f", container).Run()
	cmd := exec.Command("docker", "run", "-d", "--name", container,
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", dbPort()),
		"-e", "POSTGRES_USER=mammoth", "-e", "POSTGRES_PASSWORD=mammoth",
		"-e", "POSTGRES_DB=mammoth",
		"postgres:16-alpine")
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("docker run postgres: %s", firstLine(out))
	}
	return false, nil
}

func stopPostgres(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

func databaseURL() string {
	return fmt.Sprintf("postgres://mammoth:mammoth@127.0.0.1:%d/mammoth?sslmode=disable", dbPort())
}

// waitDB blocks until postgres answers a ping — serve exits instantly if the
// store is unreachable, so this gates the server start.
func waitDB(ctx context.Context) error {
	db, err := sql.Open("pgx", databaseURL())
	if err != nil {
		return err
	}
	defer db.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.Ping(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres did not accept connections within 30s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ── mammoth server ──────────────────────────────────────────────────────────

func startServer(exe string, token string, dir string, logPath string) (int, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "serve", "--mode=all")
	cmd.Env = envWith(map[string]string{
		"MAMMOTH_DATABASE_URL": databaseURL(),
		"MAMMOTH_HTTP_ADDR":    fmt.Sprintf("127.0.0.1:%d", httpPort()),
		"MAMMOTH_API_TOKEN":    token,
		"MAMMOTH_MASTER_KEY":   mustSecret(),
		"MAMMOTH_MEDIA_DIR":    filepath.Join(dir, "media"),
		"MAMMOTH_EXTERNAL_URL": externalURL(),
		"MAMMOTH_PXE_ENABLED":  "false",
		// the virtual_media carrier needs a BMC-reachable media base URI —
		// with it empty the boot stage fails on "boot ISO URI is empty" (the
		// same variable scripts/agent-dev/run.sh sets). The fake BMC never
		// actually mounts, so the URI only has to be well-formed; real-media
		// flows point at a deployment kernel nfsd (ops.md §4.7).
		"MAMMOTH_MEDIA_BASE_URI": "nfs://10.0.2.2/mammoth-media",
		"MAMMOTH_LOG_FORMAT":      "console",
	})
	cmd.Stdout, cmd.Stderr = logFile, logFile
	return startDetached(cmd)
}

// waitHealthy polls /healthz until the server answers 204.
func waitHealthy(ctx context.Context) error {
	url := apiBase() + "/healthz"
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mammoth never became healthy (%s)", url)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ── qemu guests ─────────────────────────────────────────────────────────────

func qemuInstallCmd(isoPath string, disk string, serialLog string) (*exec.Cmd, func(), error) {
	cmd := exec.Command("qemu-system-x86_64",
		"-m", fmt.Sprint(guestMemMB), "-smp", fmt.Sprint(guestSMP),
		"-display", "none",
		"-serial", "file:"+serialLog,
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=disk0", disk),
		"-device", "nvme,drive=disk0,serial=sandboxdisk",
		"-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0",
		"-cdrom", isoPath, "-boot", "d", "-no-reboot")
	// qemu's own chatter joins the guest serial stream — one file per node,
	// the harness shape from scripts/agent-dev.
	f, err := os.OpenFile(serialLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = f, f
	return cmd, func() { _ = f.Close() }, nil
}

// qemuBootCmd boots the installed system from disk with the guest's ssh
// forwarded to the host.
func qemuBootCmd(disk string, serialLog string, sshPort int) (*exec.Cmd, func(), error) {
	cmd := exec.Command("qemu-system-x86_64",
		"-m", fmt.Sprint(guestMemMB), "-smp", fmt.Sprint(guestSMP),
		"-display", "none",
		"-serial", "file:"+serialLog,
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=disk0", disk),
		"-device", "nvme,drive=disk0,serial=sandboxdisk",
		"-netdev", fmt.Sprintf("user,id=n0,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=n0")
	f, err := os.OpenFile(serialLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = f, f
	return cmd, func() { _ = f.Close() }, nil
}

// ── misc ────────────────────────────────────────────────────────────────────

func mustSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is not survivable
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

func tailFile(path string, n int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
