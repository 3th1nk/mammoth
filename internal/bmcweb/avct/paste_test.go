package avct

import (
	"sync"
	"testing"
	"time"
)

// kEvent is one recorded keystroke half.
type kEvent struct {
	down  bool
	usage uint16
}

// recSink records the keystrokes the injector sends, in order.
type recSink struct {
	mu     sync.Mutex
	events []kEvent
}

func (r *recSink) KeyDown(usage uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, kEvent{true, usage})
	return nil
}

func (r *recSink) KeyUp(usage uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, kEvent{false, usage})
	return nil
}

// Pointer is part of InputSink; paste never uses it.
func (r *recSink) Pointer(buttons uint8, x, y, wheel int) error { return nil }

func (r *recSink) recorded() []kEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kEvent(nil), r.events...)
}

// gateSink blocks the first KeyDown on a channel — holds the injector mid-
// key so a second startPaste lands while one is provably running.
type gateSink struct {
	release chan struct{}
	mu      sync.Mutex
	events  []kEvent
}

func (g *gateSink) KeyDown(usage uint16) error {
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, kEvent{true, usage})
	return nil
}

func (g *gateSink) KeyUp(usage uint16) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, kEvent{false, usage})
	return nil
}

// Pointer is part of InputSink; paste never uses it.
func (g *gateSink) Pointer(buttons uint8, x, y, wheel int) error { return nil }

func (g *gateSink) recorded() []kEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]kEvent(nil), g.events...)
}

// assertBalanced fails when a press was left without its release — the
// stuck-Shift class of bug, felt by the operator long after the test.
func assertBalanced(t *testing.T, events []kEvent) {
	t.Helper()
	held := map[uint16]int{}
	for _, e := range events {
		if e.down {
			held[e.usage]++
		} else {
			held[e.usage]--
		}
	}
	for u, n := range held {
		if n != 0 {
			t.Errorf("usage 0x%02x left held ×%d", u, n)
		}
	}
}

func TestPasteCharUsageCoversPrintableASCII(t *testing.T) {
	for r := rune(0x20); r <= 0x7E; r++ {
		u, _, ok := pasteCharUsage(r)
		if !ok || u == 0 {
			t.Errorf("printable ASCII %q → 0x%02x ok=%v (every command character must type)", r, u, ok)
		}
	}
	// The backtick was the historical gap: hidGrave existed but the table
	// never wired it, so manual typing dropped it too.
	if u, shift, ok := pasteCharUsage('`'); !ok || u != hidGrave || shift {
		t.Errorf("` → 0x%02x shift=%v ok=%v, want hidGrave unshifted", u, shift, ok)
	}
	for _, r := range []rune{0x7F, 0x01, '中', 'é'} {
		if _, _, ok := pasteCharUsage(r); ok {
			t.Errorf("rune %q should be skipped", r)
		}
	}
	if u, _, ok := pasteCharUsage('\n'); !ok || u != hidEnter {
		t.Errorf("\\n → 0x%02x ok=%v, want hidEnter", u, ok)
	}
	if u, _, ok := pasteCharUsage('\t'); !ok || u != hidTab {
		t.Errorf("\\t → 0x%02x ok=%v, want hidTab", u, ok)
	}
}

func TestNormalizePaste(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{"ls\r\n", "ls\n"},     // Windows copy
		{"ls\r", "ls\n"},       // lone CR
		{"a\rb\nc", "a\nb\nc"}, // mixed — no double Enter
	} {
		if got := string(normalizePaste([]byte(c.in))); got != c.want {
			t.Errorf("normalize %q = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPasteSequence(t *testing.T) {
	sink := &recSink{}
	c := &rfbClient{bridge: &RFBBridge{Input: sink, Logger: LogFunc(t.Logf)}}
	c.pasteLoop([]byte("Hi\r\n"), make(chan struct{}))
	want := []kEvent{
		// 'H': Shift rides with the base key (h), released around it (LIFO).
		{true, hidLeftShift}, {true, hidA + 7}, {false, hidA + 7}, {false, hidLeftShift},
		// 'i' — unshifted
		{true, hidA + 8}, {false, hidA + 8},
		// '\n' (from the folded CRLF)
		{true, hidEnter}, {false, hidEnter},
	}
	got := sink.recorded()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %v, want %v", i, got[i], want[i])
		}
	}
	assertBalanced(t, got)
}

func TestPasteCancelReleasesHeldKeys(t *testing.T) {
	sink := &recSink{}
	c := &rfbClient{bridge: &RFBBridge{Input: sink, Logger: LogFunc(t.Logf)}}
	cancel := make(chan struct{})
	done := make(chan struct{})
	go func() {
		c.pasteLoop([]byte("AAAAAAAAAAAAAAAAAAAA"), cancel) // shift per char, slow enough to cut mid-key
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	close(cancel)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pasteLoop did not stop after cancel")
	}
	got := sink.recorded()
	assertBalanced(t, got)
	if len(got) >= 20*4 {
		t.Errorf("cancel did not stop early: %d events for 20 chars", len(got))
	}
}

func TestPasteSingleFlight(t *testing.T) {
	gate := &gateSink{release: make(chan struct{})}
	c := &rfbClient{bridge: &RFBBridge{Input: gate, Logger: LogFunc(t.Logf)}}
	c.startPaste([]byte("abc"))
	// Deterministic without sleeping: startPaste claims pasteCancel before
	// spawning, so this call is ignored regardless of goroutine progress.
	c.startPaste([]byte("xyz"))
	close(gate.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.pasteMu.Lock()
		running := c.pasteCancel != nil
		c.pasteMu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("paste never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := []kEvent{
		{true, hidA}, {false, hidA}, // a — the gated first press
		{true, hidA + 1}, {false, hidA + 1}, // b
		{true, hidA + 2}, {false, hidA + 2}, // c
	}
	got := gate.recorded()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want exactly the first paste (%d events)", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %v, want %v", i, got[i], want[i])
		}
	}
}
