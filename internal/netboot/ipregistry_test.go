package netboot

import (
	"net"
	"testing"
	"time"
)

func TestIPRegistry(t *testing.T) {
	r := NewIPRegistry()

	// Nil-receiver safety: the executor wires the registry optionally.
	var nilReg *IPRegistry
	nilReg.Register("task-x", []string{"10.0.0.5"}, time.Minute)
	if got := nilReg.TaskForIP(net.ParseIP("10.0.0.5")); got != "" {
		t.Fatalf("nil registry resolved %q", got)
	}
	nilReg.Forget("task-x")

	// The spec's addresses are CIDR form (render.NetworkEntry): the prefix
	// must keep its host address, not be dropped by a strict ParseIP nor
	// masked down to the network address.
	r.Register("task-a", []string{"10.0.0.5/24", "10.0.0.6", "not-an-ip"}, time.Minute)
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "task-a" {
		t.Fatalf("CIDR-declared address resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.6")); got != "task-a" {
		t.Fatalf("second address resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.1")); got != "" {
		t.Fatalf("CIDR network address resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.9.9.9")); got != "" {
		t.Fatalf("unknown ip resolved %q", got)
	}

	// Re-registration (retry re-prepares) moves the addresses to the new task.
	r.Register("task-b", []string{"10.0.0.5"}, time.Minute)
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "task-b" {
		t.Fatalf("re-registration kept %q", got)
	}

	// Forget leaves a short grace window — the installer's closing syslog
	// lines race the release path — then lapses.
	r.Register("task-c", []string{"10.0.0.7"}, time.Minute)
	r.grace = 20 * time.Millisecond
	r.Forget("task-b")
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "task-b" {
		t.Fatalf("grace window did not resolve: %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.7")); got != "task-c" {
		t.Fatalf("forget hit a foreign task: %q", got)
	}
	time.Sleep(40 * time.Millisecond)
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "" {
		t.Fatalf("post-grace attribution resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.7")); got != "task-c" {
		t.Fatalf("grace shrink hit a foreign task: %q", got)
	}

	// Expiry: a shrunken TTL lapses.
	r.Register("task-d", []string{"10.0.0.8"}, time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if got := r.TaskForIP(net.ParseIP("10.0.0.8")); got != "" {
		t.Fatalf("expired attribution resolved %q", got)
	}

	// Empty inputs are no-ops.
	r.Register("", []string{"10.0.0.9"}, time.Minute)
	r.Register("task-e", nil, time.Minute)
	if got := r.TaskForIP(net.ParseIP("10.0.0.9")); got != "" {
		t.Fatalf("empty taskID registered: %q", got)
	}
}

func TestAttributionIP(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.0.0.5", "10.0.0.5"},    // bare address passes through
		{"10.0.0.5/24", "10.0.0.5"}, // CIDR keeps the host address
		{"192.168.1.221/24", "192.168.1.221"},
		{"::1", "::1"},            // v6 bare
		{"fd00::5/64", "fd00::5"}, // v6 CIDR
		{"not-an-ip", ""},         // garbage is unownable
		{"10.0.0.5/99", ""},       // malformed prefix
		{"", ""},
	}
	for _, c := range cases {
		if got := attributionIP(c.in); got != c.want {
			t.Errorf("attributionIP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
