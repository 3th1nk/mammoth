package provision

import (
	"os"
	"path/filepath"
	"testing"
)

// The boot-two verdict marker (方案 A): absent → pending; verdict rides the
// FIRST line's prefix, logs trail behind for the post-mortem.
func TestBcdbootMarkerState(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{MediaDir: dir}
	const taskID = "tsk-1"
	diagDir := filepath.Join(dir, "diag", taskID)
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(diagDir, "bcdboot-done.txt")
	write := func(content string) {
		if err := os.WriteFile(marker, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if st, _ := e.bcdbootMarkerState(t.Context(), taskID); st != bcdbootPending {
		t.Errorf("missing marker = %v, want pending", st)
	}
	write("BCDBOOT OK\nSuccess Operation.\n\n--- bcdedit /enum all ---\nWindows Boot Manager\n")
	if st, body := e.bcdbootMarkerState(t.Context(), taskID); st != bcdbootMarkerOK {
		t.Errorf("OK marker = %v, want ok (body %q)", st, body)
	}
	write("BCDBOOT FAIL bcdboot-rc=1 bcdedit-rc=0\nFailure when attempting to copy boot files.\n")
	st, body := e.bcdbootMarkerState(t.Context(), taskID)
	if st != bcdbootMarkerFailed {
		t.Errorf("FAIL marker = %v, want failed", st)
	}
	if got := firstLine(body); got != "BCDBOOT FAIL bcdboot-rc=1 bcdedit-rc=0" {
		t.Errorf("firstLine = %q", got)
	}
	write("warming up")
	if st, _ := e.bcdbootMarkerState(t.Context(), taskID); st != bcdbootPending {
		t.Errorf("prefix-less marker = %v, want pending", st)
	}

	// no MediaDir configured: pending, never a crash
	empty := &Executor{}
	if st, _ := empty.bcdbootMarkerState(t.Context(), taskID); st != bcdbootPending {
		t.Errorf("no-MediaDir = %v, want pending", st)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Errorf("firstLine multi = %q", got)
	}
	if got := firstLine("  single  "); got != "single" {
		t.Errorf("firstLine single = %q", got)
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine empty = %q", got)
	}
}
