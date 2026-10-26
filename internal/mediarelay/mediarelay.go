// Package mediarelay uploads assembled boot media to an SSH-reachable export
// directory so BMC virtual media can mount it (the media-relay deployment,
// docs/compat/huawei.md: the mammoth host builds the media; the BMC fetches
// it from a remote NFS export). Transfers are atomic — a dot-prefixed temp
// name during upload, renamed into place on completion — so the BMC never
// sees a partial image (its mount validation only reads the header; a
// partial read means a dead CD boot, see the compat notes).
package mediarelay

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Relay is one SSH endpoint exposing the media export directory.
type Relay struct {
	Addr     string // host[:port], port defaults to 22
	User     string
	Password string
	Dir      string // remote export directory
	Timeout  time.Duration
}

// New validates a relay configuration; nil parts make it unusable.
func New(addr, user, password, dir string, timeout time.Duration) (*Relay, error) {
	if strings.TrimSpace(addr) == "" || strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("mediarelay: addr and dir are required")
	}
	if user == "" {
		user = "root"
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Relay{Addr: addr, User: user, Password: password, Dir: dir, Timeout: timeout}, nil
}

// Push uploads localPath under the export directory as relName (repo-relative;
// subdirectories are created) and returns the remote file name.
func (r *Relay) Push(ctx context.Context, localPath, relName string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("mediarelay: %w", err)
	}
	defer f.Close()

	client, err := r.dial(ctx)
	if err != nil {
		return "", err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("mediarelay: session: %w", err)
	}
	defer session.Close()

	session.Stdin = f
	// One shot: stream into the temp name, rename into place atomically.
	// cat lands the bytes in the TEMP name and mv publishes it — the old
	// shape wrote the final name directly and then mv-ed a never-created
	// temp onto it: every push transferred fully and died at mv (exit 1).
	if err := session.Run(pushCommand(r.Dir, relName)); err != nil {
		return "", fmt.Errorf("mediarelay: push %s: %w", relName, err)
	}
	return relName, nil
}

// pushCommand builds the remote one-shot: mkdir the subtree, stream stdin
// into the temp name, publish by rename.
func pushCommand(dir, relName string) string {
	remote := dir + "/" + relName
	tmp := dir + "/." + filepath.Base(relName) + ".part"
	return fmt.Sprintf("mkdir -p %q && cat > %q && mv %q %q", filepath.Dir(remote), tmp, tmp, remote)
}

// Remove deletes a file from the export directory (best-effort callers log).
func (r *Relay) Remove(ctx context.Context, name string) error {
	client, err := r.dial(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("mediarelay: session: %w", err)
	}
	defer session.Close()
	if err := session.Run(fmt.Sprintf("rm -f %q", r.Dir+"/"+name)); err != nil {
		return fmt.Errorf("mediarelay: remove %s: %w", name, err)
	}
	return nil
}

func (r *Relay) dial(ctx context.Context) (*ssh.Client, error) {
	host := r.Addr
	if !strings.Contains(host, ":") {
		host += ":22"
	}
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("mediarelay: dial %s: %w", host, err)
	}
	cfg := &ssh.ClientConfig{
		User:            r.User,
		Auth:            []ssh.AuthMethod{ssh.Password(r.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // relay endpoints are deployment-internal; strict pinning is a deploy-layer policy (cf. inbandssh)
		Timeout:         30 * time.Second,
	}
	client, chans, reqs, err := ssh.NewClientConn(conn, host, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mediarelay: ssh %s@%s: %w", r.User, host, err)
	}
	go ssh.DiscardRequests(reqs)
	_ = chans
	return ssh.NewClient(client, chans, reqs), nil
}
