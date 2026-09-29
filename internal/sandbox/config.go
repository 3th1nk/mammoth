// Package sandbox runs a self-contained local provisioning sandbox:
// `mammoth sandbox up` starts postgres, a mammoth server and N qemu guests
// backed by fake BMCs, drives a full alpine install through the default
// virtual_media carrier, then boots the installed systems for SSH.
//
// The topology is the productized shape of scripts/agent-dev (verified
// end-to-end on TCG): every guest is an isolated slirp NAT, mammoth owns no
// PXE UDP ports, and docker supplies only postgresql. No privileged
// containers, no bridges, no root.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultNodes = 3
	MaxNodes     = 5

	container  = "mammoth-sandbox-db"
	alpineVer  = "3.22.2"
	alpineISO  = "alpine-extended-" + alpineVer + "-x86_64.iso"
	guestMemMB = 2048
	guestSMP   = 2
	diskGB     = 8
	// nfsExportPort: the in-server NFS media export. 2049 belongs to the
	// system nfsd on most macs, so the sandbox moves off it.
	nfsExportPort = 12049

	prepareMedia = 10 * time.Minute // boot ISO rendered per task
	installWait  = 25 * time.Minute // six stages, 3 guests on TCG
	bootWait     = 2 * time.Minute  // installed system until sshd answers
)

// Ports are fixed by default so the printed ssh/API instructions are stable,
// but every one is overridable: real laptops have 18080-class occupants.
// MAMMOTH_SANDBOX_HTTP_PORT (18080), MAMMOTH_SANDBOX_DB_PORT (55433),
// MAMMOTH_SANDBOX_SSH_PORT (2222; node i → base+i).
func envPort(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return def
}

func httpPort() int  { return envPort("MAMMOTH_SANDBOX_HTTP_PORT", 18080) }
func dbPort() int    { return envPort("MAMMOTH_SANDBOX_DB_PORT", 55433) }
func sshBase() int   { return envPort("MAMMOTH_SANDBOX_SSH_PORT", 2222) }

// Dir is the sandbox state directory: state.json, the one-shot SSH key, the
// alpine ISO cache, guest disks and logs. Assets survive `sandbox down`.
// MAMMOTH_SANDBOX_DIR overrides the default (~/.mammoth-sandbox).
func Dir() string {
	if d := os.Getenv("MAMMOTH_SANDBOX_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mammoth-sandbox"
	}
	return filepath.Join(home, ".mammoth-sandbox")
}

// alpineISOURL pins the extended ISO on the official CDN; extended is required
// because only it ships linux-lts/grub/mkinitfs in /apks (docs/12-agent-initramfs).
func alpineISOURL() string {
	minor := alpineVer[:strings.LastIndex(alpineVer, ".")] // 3.22.2 → 3.22
	return fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/releases/x86_64/%s", minor, alpineISO)
}

// apiBase is where the sandboxed mammoth serves HTTP.
func apiBase() string { return fmt.Sprintf("http://127.0.0.1:%d", httpPort()) }

// externalURL is baked into the rendered boot ISO; guests reach the host's
// mammoth through the slirp gateway (10.0.2.2), never through 127.0.0.1.
func externalURL() string { return fmt.Sprintf("http://10.0.2.2:%d", httpPort()) }
