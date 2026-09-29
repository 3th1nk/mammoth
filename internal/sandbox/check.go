package sandbox

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
)

// dependency is one host tool the sandbox shells out to. xorriso is not
// optional: the render pipeline (internal/builder) extracts ISO trees through
// it, for virtual_media as much as for PXE.
type dependency struct {
	name string
	hint string
}

func dependencies() []dependency {
	brew := "brew install qemu xorriso"
	apt := "sudo apt-get install -y qemu-system-x86 qemu-utils xorriso"
	hint := brew
	if runtime.GOOS == "linux" {
		hint = apt
	}
	return []dependency{
		{"qemu-system-x86_64", hint},
		{"qemu-img", hint},
		{"xorriso", hint},
		{"docker", "install docker (https://docs.docker.com/get-docker/)"},
		{"curl", hint},
		{"ssh-keygen", hint},
		{"ssh", hint},
	}
}

// CheckDependencies verifies every host tool exists and the fixed port set is
// free for the requested node count. It returns a ready-to-print error naming
// the first missing tool and its install hint.
func CheckDependencies(nodes int) error {
	for _, d := range dependencies() {
		if _, err := exec.LookPath(d.name); err != nil {
			return fmt.Errorf("sandbox needs %s — install it with: %s", d.name, d.hint)
		}
	}
	if err := dockerAlive(); err != nil {
		return fmt.Errorf("docker is installed but not usable: %w", err)
	}
	for _, port := range requiredPorts(nodes) {
		if inUse(port) {
			return fmt.Errorf("port %d is taken — stop the occupant or pick another base port via MAMMOTH_SANDBOX_HTTP_PORT / MAMMOTH_SANDBOX_DB_PORT / MAMMOTH_SANDBOX_SSH_PORT", port)
		}
	}
	return nil
}

func dockerAlive() error {
	out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker info failed: %s", firstLine(out))
	}
	return nil
}

// requiredPorts: the API port, the postgres port and one ssh forward per node.
func requiredPorts(nodes int) []int {
	ports := []int{httpPort(), dbPort()}
	for i := 0; i < nodes; i++ {
		ports = append(ports, sshBase()+i)
	}
	return ports
}

func inUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

func firstLine(b []byte) string {
	for i := range b {
		if b[i] == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
