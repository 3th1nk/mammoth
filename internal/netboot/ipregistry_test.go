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

	r.Register("task-a", []string{"10.0.0.5", "10.0.0.6", "not-an-ip"}, time.Minute)
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "task-a" {
		t.Fatalf("registered ip resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.6")); got != "task-a" {
		t.Fatalf("second address resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.6")); got != "task-a" { // string form stable
		_ = got
	}
	if got := r.TaskForIP(net.ParseIP("10.9.9.9")); got != "" {
		t.Fatalf("unknown ip resolved %q", got)
	}

	// Re-registration (retry re-prepares) moves the addresses to the new task.
	r.Register("task-b", []string{"10.0.0.5"}, time.Minute)
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "task-b" {
		t.Fatalf("re-registration kept %q", got)
	}

	// Forget drops exactly the task's own addresses.
	r.Register("task-c", []string{"10.0.0.7"}, time.Minute)
	r.Forget("task-b")
	if got := r.TaskForIP(net.ParseIP("10.0.0.5")); got != "" {
		t.Fatalf("forgotten task resolved %q", got)
	}
	if got := r.TaskForIP(net.ParseIP("10.0.0.7")); got != "task-c" {
		t.Fatalf("forget hit a foreign task: %q", got)
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
