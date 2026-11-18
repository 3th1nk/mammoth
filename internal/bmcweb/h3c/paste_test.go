package h3c

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// capLog captures bridge logs so tests can await paste milestones.
type capLog struct {
	mu sync.Mutex
	ls []string
}

func (l *capLog) Printf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ls = append(l.ls, fmt.Sprintf(f, a...))
}

func (l *capLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(strings.Join(l.ls, "\n"), sub)
}

func (l *capLog) waitFor(sub string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if l.has(sub) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestPasteKeysymCoversPrintableASCII(t *testing.T) {
	for r := rune(0x20); r <= 0x7E; r++ {
		sym, ok := pasteKeysym(r)
		if !ok || sym != uint32(r) {
			t.Errorf("printable ASCII %q → 0x%x ok=%v (keysym must be the char itself)", r, sym, ok)
		}
	}
	if sym, ok := pasteKeysym('\n'); !ok || sym != 0xFF0D {
		t.Errorf("\\n → 0x%x ok=%v, want Enter", sym, ok)
	}
	if sym, ok := pasteKeysym('\t'); !ok || sym != 0xFF09 {
		t.Errorf("\\t → 0x%x ok=%v, want Tab", sym, ok)
	}
	for _, r := range []rune{0x7F, 0x01, '中', 'é'} {
		if _, ok := pasteKeysym(r); ok {
			t.Errorf("rune %q should be skipped", r)
		}
	}
}

func TestNormalizePasteH3C(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ls\r\n", "ls\n"},
		{"a\rb\nc", "a\nb\nc"},
	} {
		if got := string(normalizePaste([]byte(c.in))); got != c.want {
			t.Errorf("normalize %q = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeDeviceHandshake speaks the device side of RFB 3.8 with VNC auth and
// verifies the bridge answered the DES challenge with the credential
// password — then hands the live connection to the test.
func fakeDeviceHandshake(t *testing.T, ln net.Listener, password string) net.Conn {
	t.Helper()
	dev, err := ln.Accept()
	if err != nil {
		t.Fatalf("device accept: %v", err)
	}
	t.Cleanup(func() { dev.Close() })
	if _, err := dev.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(dev, ver); err != nil {
		t.Fatal(err)
	}
	if _, err := dev.Write([]byte{1, 2}); err != nil { // one type: VNC auth
		t.Fatal(err)
	}
	choice := make([]byte, 1)
	if _, err := io.ReadFull(dev, choice); err != nil {
		t.Fatal(err)
	}
	if choice[0] != 2 {
		t.Fatalf("bridge picked security type %d, want VNC (2)", choice[0])
	}
	chal := bytes.Repeat([]byte{0xA5, 0x5A, 0x37}, 6)[:16] // fixed challenge
	if _, err := dev.Write(chal); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(dev, resp); err != nil {
		t.Fatal(err)
	}
	if want := rfbVNCResponse(password, chal); !bytes.Equal(resp, want) {
		t.Fatalf("DES response = %x, want %x", resp, want)
	}
	if _, err := dev.Write([]byte{0, 0, 0, 0}); err != nil { // result OK
		t.Fatal(err)
	}
	ci := make([]byte, 1)
	if _, err := io.ReadFull(dev, ci); err != nil { // ClientInit
		t.Fatal(err)
	}
	if ci[0] != 1 {
		t.Fatalf("ClientInit = %d", ci[0])
	}
	var si []byte
	si = binary.BigEndian.AppendUint16(si, 64) // width
	si = binary.BigEndian.AppendUint16(si, 32) // height
	si = append(si, make([]byte, 16)...)       // pixel format (ignored by the test)
	si = binary.BigEndian.AppendUint32(si, 5)  // name length
	si = append(si, "hdmvd"...)
	if _, err := dev.Write(si); err != nil {
		t.Fatal(err)
	}
	return dev
}

// clientHandshake plays noVNC's side against the bridge.
func clientHandshake(t *testing.T, ws net.Conn) {
	t.Helper()
	ver := make([]byte, 12)
	if _, err := io.ReadFull(ws, ver); err != nil {
		t.Fatalf("client read version: %v", err)
	}
	if _, err := ws.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	sec := make([]byte, 2)
	if _, err := io.ReadFull(ws, sec); err != nil {
		t.Fatal(err)
	}
	if sec[0] != 1 || sec[1] != 1 {
		t.Fatalf("client was offered %v, want [None]", sec)
	}
	if _, err := ws.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	res := make([]byte, 4)
	if _, err := io.ReadFull(ws, res); err != nil {
		t.Fatal(err)
	}
	if res[0] != 0 || res[1] != 0 || res[2] != 0 || res[3] != 0 {
		t.Fatalf("client saw security result %x", res)
	}
	if _, err := ws.Write([]byte{1}); err != nil { // ClientInit
		t.Fatal(err)
	}
	si := make([]byte, 24)
	if _, err := io.ReadFull(ws, si); err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(si[20:])
	name := make([]byte, n)
	if _, err := io.ReadFull(ws, name); err != nil {
		t.Fatal(err)
	}
	if string(name) != "hdmvd" {
		t.Fatalf("client saw server name %q", name)
	}
}

func cutText(ws net.Conn, text string) error {
	b := []byte{6, 0, 0, 0}
	b = binary.BigEndian.AppendUint32(b, uint32(len(text)))
	b = append(b, text...)
	_, err := ws.Write(b)
	return err
}

func keyEventMsg(ws net.Conn, sym uint32, down bool) error {
	b := []byte{4, 0, 0, 0}
	if down {
		b[1] = 1
	}
	b = binary.BigEndian.AppendUint32(b, sym)
	_, err := ws.Write(b)
	return err
}

// readKeyEvent reads one KeyEvent (type 4) frame from the device side.
func readKeyEvent(t *testing.T, dev net.Conn) (sym uint32, down bool) {
	t.Helper()
	var tb [1]byte
	if _, err := io.ReadFull(dev, tb[:]); err != nil {
		t.Fatalf("device read message type: %v", err)
	}
	if tb[0] != 4 {
		t.Fatalf("device got message type %d, want KeyEvent", tb[0])
	}
	b := make([]byte, 7)
	if _, err := io.ReadFull(dev, b); err != nil {
		t.Fatalf("device read keyevent: %v", err)
	}
	return binary.BigEndian.Uint32(b[3:]), b[0] != 0
}

func TestBridgeRFBPasteAndFilter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ws, wst := net.Pipe()
	defer wst.Close()
	log := &capLog{}
	served := make(chan error, 1)
	go func() { served <- BridgeRFB(ws, ln.Addr().String(), "vncpass", log) }()

	devc := make(chan net.Conn, 1)
	go func() { devc <- fakeDeviceHandshake(t, ln, "vncpass") }()
	clientHandshake(t, wst)
	var dev net.Conn
	select {
	case dev = <-devc:
	case <-time.After(5 * time.Second):
		t.Fatal("device handshake never completed")
	}

	// ── forwarded messages stay byte-faithful ──
	if err := keyEventMsg(wst, 0x61, true); err != nil { // manual key press
		t.Fatal(err)
	}
	if sym, down := readKeyEvent(t, dev); !down || sym != 0x61 {
		t.Fatalf("forwarded key = 0x%x down=%v", sym, down)
	}
	if _, err := wst.Write(append([]byte{5}, make([]byte, 5)...)); err != nil { // PointerEvent
		t.Fatal(err)
	}
	var tb [1]byte
	if _, err := io.ReadFull(dev, tb[:]); err != nil || tb[0] != 5 {
		t.Fatalf("pointer forward: type %d err %v", tb[0], err)
	}
	rest := make([]byte, 5)
	if _, err := io.ReadFull(dev, rest); err != nil {
		t.Fatal(err)
	}

	// ── paste: "aB\n" → six KeyEvents with the case carried by the keysym ──
	if err := cutText(wst, "aB\n"); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		sym  uint32
		down bool
	}{
		{0x61, true}, {0x61, false},
		{0x42, true}, {0x42, false}, // 'B' — keysym itself carries the shift
		{0xFF0D, true}, {0xFF0D, false}, // Enter
	}
	for i, w := range want {
		sym, down := readKeyEvent(t, dev)
		if sym != w.sym || down != w.down {
			t.Fatalf("paste key %d = 0x%x down=%v, want 0x%x down=%v", i, sym, down, w.sym, w.down)
		}
	}
	if !log.waitFor("typed 3 chars, skipped 0", 5*time.Second) {
		t.Fatalf("paste log missing: %v", log.ls)
	}

	// ── cancel: a real key press stops a running injection mid-flight ──
	if err := cutText(wst, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := keyEventMsg(wst, 'x', true); err != nil { // cancels AND forwards
		t.Fatal(err)
	}
	if err := keyEventMsg(wst, 'x', false); err != nil {
		t.Fatal(err)
	}
	if !log.waitFor("canceled after", 5*time.Second) {
		t.Fatalf("cancel never fired: %v", log.ls)
	}
	// Drain the device side briefly: the injector stops fast, and every
	// key it already sent must be released (no stuck keys).
	drain := make(map[uint32]int)
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		_ = dev.SetReadDeadline(deadline)
		var hb [1]byte
		if _, err := io.ReadFull(dev, hb[:]); err != nil {
			break // quiet
		}
		if hb[0] != 4 {
			t.Fatalf("unexpected message type %d during drain", hb[0])
		}
		b := make([]byte, 7)
		if _, err := io.ReadFull(dev, b); err != nil {
			break
		}
		sym := binary.BigEndian.Uint32(b[3:])
		if b[0] != 0 {
			drain[sym]++
		} else {
			drain[sym]--
		}
	}
	_ = dev.SetReadDeadline(time.Time{})
	for sym, n := range drain {
		if n != 0 {
			t.Errorf("usage 0x%x left held ×%d after cancel", sym, n)
		}
	}
	if n := log.canceledCount(); n < 0 || n >= 40 {
		t.Fatalf("canceled count %d — want mid-flight", n)
	}

	// ── single-flight: the second paste is ignored while one runs ──
	if err := cutText(wst, strings.Repeat("b", 30)); err != nil {
		t.Fatal(err)
	}
	if err := cutText(wst, "second"); err != nil { // readLoop is serial: first claimed the slot
		t.Fatal(err)
	}
	if !log.waitFor("one already running", 5*time.Second) {
		t.Fatalf("single-flight did not reject: %v", log.ls)
	}
	if err := keyEventMsg(wst, 'y', true); err != nil {
		t.Fatal(err)
	}
	if err := keyEventMsg(wst, 'y', false); err != nil {
		t.Fatal(err)
	}
	if !log.waitFor("canceled after", 5*time.Second) {
		t.Fatalf("second cancel never fired: %v", log.ls)
	}

	_ = wst.Close()
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("bridge returned nil after viewer close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop after viewer close")
	}
}

func (l *capLog) canceledCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := -1
	for _, s := range l.ls {
		var v int
		if _, err := fmt.Sscanf(s, "rfb: paste: canceled after %d/", &v); err == nil {
			n = v
		}
	}
	return n
}

// readOneMessage reads exactly one client message the filter forwarded and
// returns its full wire bytes (device side).
func readOneMessage(t *testing.T, dev net.Conn) []byte {
	t.Helper()
	var tb [1]byte
	if _, err := io.ReadFull(dev, tb[:]); err != nil {
		t.Fatalf("device read type: %v", err)
	}
	sizes := map[byte]int{0: 19, 3: 9, 4: 7, 5: 5}
	if n, ok := sizes[tb[0]]; ok {
		b := make([]byte, n)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatalf("device read body: %v", err)
		}
		return append([]byte{tb[0]}, b...)
	}
	switch tb[0] {
	case 2: // pad+count, then 4n
		b := make([]byte, 3)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatal(err)
		}
		n := int(binary.BigEndian.Uint16(b[1:]))
		enc := make([]byte, n*4)
		if _, err := io.ReadFull(dev, enc); err != nil {
			t.Fatal(err)
		}
		return append(append([]byte{2}, b...), enc...)
	case 6: // cut text — must NEVER arrive (intercepted)
		t.Fatal("ClientCutText leaked to the device")
	case 150: // enable continuous updates
		b := make([]byte, 12)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatal(err)
		}
		return append([]byte{150}, b...)
	case 248: // fence: pad3 flags4 len1 payload
		b := make([]byte, 8)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, b[7])
		if _, err := io.ReadFull(dev, p); err != nil {
			t.Fatal(err)
		}
		return append(append([]byte{248}, b...), p...)
	case 250: // xvp
		b := make([]byte, 2)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatal(err)
		}
		return append([]byte{250}, b...)
	case 251: // set desktop size
		b := make([]byte, 6)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatal(err)
		}
		s := make([]byte, int(b[4])*16)
		if _, err := io.ReadFull(dev, s); err != nil {
			t.Fatal(err)
		}
		return append(append([]byte{251}, b...), s...)
	case 255: // QEMU extended
		sub := make([]byte, 1)
		if _, err := io.ReadFull(dev, sub); err != nil {
			t.Fatal(err)
		}
		n := 10 // keyboard: down(2)+keysym(4)+keycode(4)
		if sub[0] == 1 {
			n = 7 // pointer: pad(2)+mask(1)+x(2)+y(2)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(dev, b); err != nil {
			t.Fatal(err)
		}
		return append(append([]byte{255}, sub...), b...)
	}
	t.Fatalf("device got unexpected message type %d", tb[0])
	return nil
}

// TestBridgeRFBViewerExtensions drives the filter with the extension
// message set a real noVNC sends once the AMI HDM acknowledges the QEMU
// extended keyboard pseudo-encoding: from then on keystrokes ride message
// type 255, and a filter that drops or mangles them kills the session on
// the operator's first key press (the v1.5.0-10 regression).
func TestBridgeRFBViewerExtensions(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ws, wst := net.Pipe()
	defer wst.Close()
	log := &capLog{}
	go func() { _ = BridgeRFB(ws, ln.Addr().String(), "vncpass", log) }()

	devc := make(chan net.Conn, 1)
	go func() { devc <- fakeDeviceHandshake(t, ln, "vncpass") }()
	clientHandshake(t, wst)
	var dev net.Conn
	select {
	case dev = <-devc:
	case <-time.After(5 * time.Second):
		t.Fatal("device handshake never completed")
	}

	// QEMU extended keyboard: type 255, sub 0, down u16, keysym, keycode.
	qemuKey := func(sym uint32, down bool) {
		b := []byte{255, 0}
		if down {
			b = binary.BigEndian.AppendUint16(b, 1)
		} else {
			b = binary.BigEndian.AppendUint16(b, 0)
		}
		b = binary.BigEndian.AppendUint32(b, sym)
		b = binary.BigEndian.AppendUint32(b, 0x26) // XT scancode for 'a'-ish
		if _, err := wst.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	wantQEMU := func(sym uint32, down bool) {
		t.Helper()
		got := readOneMessage(t, dev)
		want := []byte{255, 0}
		if down {
			want = binary.BigEndian.AppendUint16(want, 1)
		} else {
			want = binary.BigEndian.AppendUint16(want, 0)
		}
		want = binary.BigEndian.AppendUint32(want, sym)
		want = binary.BigEndian.AppendUint32(want, 0x26)
		if !bytes.Equal(got, want) {
			t.Fatalf("forwarded QEMU key = % x, want % x", got, want)
		}
	}

	qemuKey(0xFFBF, true) // F2 down
	wantQEMU(0xFFBF, true)
	qemuKey(0xFFBF, false)
	wantQEMU(0xFFBF, false)

	// Fence and continuous updates ride through byte-faithful too.
	fence := []byte{248, 0, 0, 0, 0, 0, 0, 1, 3, 'a', 'b', 'c'}
	if _, err := wst.Write(fence); err != nil {
		t.Fatal(err)
	}
	if got := readOneMessage(t, dev); !bytes.Equal(got, fence) {
		t.Fatalf("forwarded fence = % x, want % x", got, fence)
	}
	cont := []byte{150, 1, 0, 0, 0, 0, 0, 10, 0, 20, 0, 30, 0}
	if _, err := wst.Write(cont); err != nil {
		t.Fatal(err)
	}
	if got := readOneMessage(t, dev); !bytes.Equal(got, cont) {
		t.Fatalf("forwarded continuous-updates = % x, want % x", got, cont)
	}

	// And the QEMU down event cancels a running paste — the real-viewer
	// cancel path, distinct from the plain-KeyEvent one covered above.
	if err := cutText(wst, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// The injector is mid-typing plain KeyEvents when the QEMU frames land;
	// its tail frames interleave, so wait for the exact frames instead of
	// asserting strict order.
	waitQEMU := func(sym uint32, down bool) {
		t.Helper()
		want := []byte{255, 0}
		if down {
			want = binary.BigEndian.AppendUint16(want, 1)
		} else {
			want = binary.BigEndian.AppendUint16(want, 0)
		}
		want = binary.BigEndian.AppendUint32(want, sym)
		want = binary.BigEndian.AppendUint32(want, 0x26)
		for i := 0; i < 300; i++ {
			got := readOneMessage(t, dev)
			if bytes.Equal(got, want) {
				return
			}
			if got[0] == 255 { // QEMU frames come only from this test
				t.Fatalf("unexpected QEMU frame % x", got)
			}
		}
		t.Fatalf("QEMU frame % x never forwarded", want)
	}
	qemuKey('x', true)
	waitQEMU('x', true)
	qemuKey('x', false)
	waitQEMU('x', false)
	if !log.waitFor("canceled after", 5*time.Second) {
		t.Fatalf("QEMU key press did not cancel the paste: %v", log.ls)
	}
}
