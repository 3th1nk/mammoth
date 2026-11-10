package netboot

// The address-service observer's three signals (docs/09-roadmap.md「装机网
// 段地址服务可观测」): discover-repeat fires the root-cause line at the
// threshold; an addressing REQUEST clears the state (healthy contrast);
// the installed-orphan arms after the terminal note and attributes to the
// terminal task's log.

import (
	"bytes"
	"log/slog"
	"testing"
	"time"
)

func newTestObserver() (*Observer, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	o := NewObserver(func(mac string) string {
		if mac == "aa:bb:cc:dd:ee:01" {
			return "tsk_armed"
		}
		return ""
	}, log)
	o.now = func() time.Time { return time.Unix(1_000_000, 0) } // fixed; tests advance manually
	return o, &buf
}

func TestDiscoverRepeatFiresAtThreshold(t *testing.T) {
	o, buf := newTestObserver()
	// two DISCOVERs: below threshold, silent
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	if got := buf.String(); got != "" {
		t.Fatalf("below threshold must stay silent, got: %s", got)
	}
	// third: the root-cause line, attributed to the armed task
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("no DHCP answered")) || !bytes.Contains(buf.Bytes(), []byte("tsk_armed")) {
		t.Fatalf("threshold must fire attributed root cause, got: %s", out)
	}
	// further DISCOVERs inside the suppression window stay quiet
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	if len(bytes.Split(buf.Bytes(), []byte("\n"))) > 3 { // line + trailing empty
		t.Fatalf("suppression window must hold, got: %s", buf.String())
	}
}

func TestAddressedRequestClearsState(t *testing.T) {
	o, buf := newTestObserver()
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	// the healthy contrast: a REQUEST carrying an address claim (option 50
	// already handled by the caller) — the state clears
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgRequest, true)
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	if buf.String() != "" {
		t.Fatalf("addressed REQUEST must clear the repeat state, got: %s", buf.String())
	}
}

func TestForeignMACStaysUnattributed(t *testing.T) {
	o, buf := newTestObserver()
	for i := 0; i < discoverThreshold; i++ {
		o.NoteMessage("de:ad:be:ef:00:01", msgDiscover, false)
	}
	if !bytes.Contains(buf.Bytes(), []byte("no task attribution")) {
		t.Fatalf("foreign MAC must log without attribution, got: %s", buf.String())
	}
}

func TestInstalledOrphanArmsAfterTerminal(t *testing.T) {
	o, buf := newTestObserver()
	o.NoteTerminal("aa:bb:cc:dd:ee:01", "tsk_done")
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("installed machine keeps discovering")) ||
		!bytes.Contains(buf.Bytes(), []byte("tsk_done")) {
		t.Fatalf("terminal MAC's discover must fire the orphan line on the terminal task, got: %s", out)
	}
	// suppression holds
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	if bytes.Count(buf.Bytes(), []byte("installed machine")) != 1 {
		t.Fatalf("orphan line must be suppressed after the first, got: %s", buf.String())
	}
}

func TestWindowResetsBurst(t *testing.T) {
	o, buf := newTestObserver()
	base := time.Unix(1_000_000, 0)
	o.now = func() time.Time { return base }
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	// outside the window the burst resets — two more never fire
	o.now = func() time.Time { return base.Add(discoverWindow + time.Minute) }
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	if buf.String() != "" {
		t.Fatalf("out-of-window burst must reset, got: %s", buf.String())
	}
}

// 2026-11-10 .132 real-segment panic: serve constructs the observer before
// its resolver exists (taskFor nil), and the first real DISCOVER burst past
// the threshold called the nil func — SIGSEGV took the whole netboot
// handler down. Attribution must degrade, never panic.
func TestNilTaskForDegradesInsteadOfPanic(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	o := NewObserver(nil, log)
	o.now = func() time.Time { return time.Unix(1_000_000, 0) }
	for i := 0; i < discoverThreshold; i++ {
		o.NoteMessage("aa:bb:cc:dd:ee:01", msgDiscover, false)
	}
	if !bytes.Contains(buf.Bytes(), []byte("no task attribution")) {
		t.Fatalf("nil taskFor must degrade to engine-log-only, got: %s", buf.String())
	}
	// Late wiring turns attribution on without restarting the watcher.
	o.SetTaskFor(func(mac string) string { return "tsk_late" })
	for i := 0; i < discoverThreshold; i++ {
		o.NoteMessage("aa:bb:cc:dd:ee:02", msgDiscover, false)
	}
	if !bytes.Contains(buf.Bytes(), []byte("tsk_late")) {
		t.Fatalf("SetTaskFor must arm attribution, got: %s", buf.String())
	}
}
