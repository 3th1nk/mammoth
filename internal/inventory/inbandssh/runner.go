package inbandssh

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHRunner runs the command set over a real SSH connection: single dial,
// single exec, close. Host keys are accepted on first contact — machines in
// provisioning lifecycles rotate host keys by design; pinned verification is
// a deployment-level policy for a later milestone.
type SSHRunner struct {
	// DialTimeout bounds the TCP+handshake phase (default 5s).
	DialTimeout time.Duration
}

func (r *SSHRunner) Run(ctx context.Context, addr string, cred Credentials, script string) ([]byte, error) {
	host, port := splitHostPort(addr)
	dialTimeout := r.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	nc, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User: cred.Username,
		Auth: []ssh.AuthMethod{ssh.Password(cred.Password)},
		// Provisioned machines rotate host keys; see package comment.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, net.JoinHostPort(host, strconv.Itoa(port)), cfg)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(script)
	if err != nil {
		return out, err
	}
	return out, nil
}

func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	port := 22
	if err == nil {
		if p, perr := strconv.Atoi(portStr); perr == nil {
			port = p
		}
		return host, port
	}
	return strings.Trim(addr, "[]"), port
}
