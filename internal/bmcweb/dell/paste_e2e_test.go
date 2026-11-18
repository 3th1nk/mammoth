package dell

// Real-hardware E2E for paste injection, gated by the same MAMMOTH_DELL_E2E
// as TestE2EConsoleBridge:
//
//	MAMMOTH_DELL_E2E='host=... user=... pass=...' MAMMOTH_PASTE_E2E_PNG=/tmp/pngs \
//	  go test ./internal/bmcweb/dell/ -run TestE2EPasteInjection -v
//
// The fake viewer stands in for noVNC over an in-memory pipe, so the
// production path is exercised against the appliance for real:
//
//	fake noVNC ──net.Pipe──▶ RFBBridge(readLoop/paste) ──avct/TLS──▶ iDRAC
//
// Steps:
//  1. ClientCutText "Mammoth-Paste_0" — the whole injector chain incl. the
//     synthesised Shift presses, ~28 keys/s against the real control channel.
//  2. Cancel: a long paste aborted by a real key press mid-flight.
//  3. Single-flight: a second paste while one is running is ignored.
//  4. Visible echo proof: F2 opens the DCUI login dialog whose Login Name
//     field echoes characters — paste into it, snapshot, eyeball the PNG
//     (goes to MAMMOTH_PASTE_E2E_PNG). Safe by construction: no Enter is
//     ever injected (ESC dismisses the dialog), so nothing executes on
//     the host.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/bmcweb/avct"
)

// capLog captures bridge logs so the test can await paste milestones.
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

// canceledOf returns the N of the last "canceled after N/M chars" line.
func (l *capLog) canceledOf() int {
	re := regexp.MustCompile(`canceled after (\d+)/`)
	n := -1
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.ls {
		if m := re.FindStringSubmatch(s); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				n = v
			}
		}
	}
	return n
}

// rfbHandshake plays the noVNC side of the bridge's 3.8 handshake.
func rfbHandshake(t *testing.T, c net.Conn) {
	t.Helper()
	ver := make([]byte, 12)
	pipeReadFull(t, c, ver)
	if !bytes.HasPrefix(ver, []byte("RFB 003.008")) {
		t.Fatalf("server version %q", ver)
	}
	if _, err := c.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	sec := make([]byte, 2)
	pipeReadFull(t, c, sec) // types {None}
	if sec[0] != 1 {
		t.Fatalf("security types %v", sec)
	}
	if _, err := c.Write([]byte{1}); err != nil { // choose None
		t.Fatal(err)
	}
	res := make([]byte, 4)
	pipeReadFull(t, c, res)                       // security result OK
	if _, err := c.Write([]byte{1}); err != nil { // ClientInit
		t.Fatal(err)
	}
	si := make([]byte, 24)
	pipeReadFull(t, c, si) // ServerInit
	w := int(binary.BigEndian.Uint16(si[0:]))
	h := int(binary.BigEndian.Uint16(si[2:]))
	nameLen := int(binary.BigEndian.Uint32(si[20:]))
	if nameLen > 0 && nameLen < 512 {
		name := make([]byte, nameLen)
		pipeReadFull(t, c, name)
		t.Logf("rfb handshake ok: %dx%d %q", w, h, name)
	}
}

func clientCutText(c net.Conn, text string) error {
	// Wire: type(1) + padding(3) + length(4) + text.
	b := []byte{6, 0, 0, 0}
	b = binary.BigEndian.AppendUint32(b, uint32(len(text)))
	b = append(b, text...)
	_, err := c.Write(b)
	return err
}

func keyEvent(c net.Conn, keysym uint32, down bool) error {
	b := []byte{4, 0, 0, 0} // type, down-flag, pad
	if down {
		b[1] = 1
	}
	b = binary.BigEndian.AppendUint32(b, keysym)
	_, err := c.Write(b)
	return err
}

func pipeReadFull(t *testing.T, c net.Conn, buf []byte) {
	t.Helper()
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		if err != nil {
			t.Fatalf("pipe read at %d/%d: %v", n, len(buf), err)
		}
		n += m
	}
}

// snapPNG dumps the framebuffer and returns the image plus its non-black
// pixel count.
func snapPNG(t *testing.T, fb *avct.Framebuffer, path string) (*image.RGBA, int) {
	t.Helper()
	img, _, _ := fb.Snapshot()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("snap %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	t.Logf("framebuffer → %s", path)
	return img, nonBlackPixels(img)
}

// diffCount counts pixels whose RGB differs beyond encoder noise.
func diffCount(a, b *image.RGBA) int {
	n := 0
	for i := 0; i+3 < len(a.Pix) && i+3 < len(b.Pix); i += 4 {
		d := abs8(a.Pix[i], b.Pix[i]) + abs8(a.Pix[i+1], b.Pix[i+1]) + abs8(a.Pix[i+2], b.Pix[i+2])
		if d > 24 {
			n++
		}
	}
	return n
}

func abs8(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func nonBlackPixels(img *image.RGBA) int {
	n := 0
	for i := 0; i+3 < len(img.Pix); i += 4 {
		if img.Pix[i] > 8 || img.Pix[i+1] > 8 || img.Pix[i+2] > 8 {
			n++
		}
	}
	return n
}

func TestE2EPasteInjection(t *testing.T) {
	host, user, pass := e2eConfig(t)
	outDir := os.Getenv("MAMMOTH_PASTE_E2E_PNG")
	if outDir == "" {
		outDir = t.TempDir()
	}
	png := func(name string) string { return filepath.Join(outDir, host+"-"+name+".png") }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Same bring-up as TestE2EConsoleBridge: web login → ticket → avct → video.
	client, err := NewClient(host, user, pass, 60*time.Second)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := client.Login(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() {
		lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer lcancel()
		_ = client.Logout(lctx)
	}()
	cc, err := client.ConsoleLaunch(ctx, "paste-e2e, User: "+user)
	if err != nil {
		t.Fatalf("console launch: %v", err)
	}
	sess := avct.NewSession(avct.Config{
		Host: cc.Host, ControlPort: cc.KMPort, VideoPort: cc.VPort,
		Username: cc.User, Password: cc.Password,
		Timeout: 45 * time.Second,
		Logger:  avct.LogFunc(t.Logf),
	})
	if err := sess.Connect(ctx); err != nil {
		t.Fatalf("avct login: %v", err)
	}
	defer sess.Close()
	fb := avct.NewFramebuffer(1024, 768)
	sess.SetVideoStream(avct.NewVideoStream(fb))
	time.Sleep(500 * time.Millisecond)
	if err := sess.OpenVideo(ctx, fb); err != nil {
		t.Fatalf("video: %v", err)
	}
	deadline := time.Now().Add(40 * time.Second)
	live := false
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if sess.State() != avct.StateRunning {
			continue
		}
		img, _, _ := fb.Snapshot()
		if nonBlackPixels(img) > 1024*768/100 {
			live = true
			break
		}
	}
	if !live {
		t.Fatalf("no video content (state %s)", sess.State())
	}

	// Production RFBBridge with the fake viewer on the other end of a pipe.
	tc, br := net.Pipe()
	defer tc.Close()
	log := &capLog{}
	bridge := &avct.RFBBridge{FB: fb, Input: sess, Name: cc.Title, Logger: avct.LogFunc(log.Printf)}
	served := make(chan error, 1)
	go func() { served <- bridge.Serve(br) }()
	rfbHandshake(t, tc)

	// ── 1. paste through the whole chain (incl. Shift synthesis) ──
	if err := clientCutText(tc, "Mammoth-Paste_0"); err != nil {
		t.Fatalf("cut text: %v", err)
	}
	if !log.waitFor("typed 15 chars", 30*time.Second) {
		t.Fatalf("paste never completed; logs: %v", log.ls)
	}
	t.Log("paste 'Mammoth-Paste_0' typed at ~28 keys/s without transport errors")
	snapPNG(t, fb, png("1-after-paste"))

	// ── 2. cancel: a real key press stops a running injection ──
	if err := clientCutText(tc, strings.Repeat("a", 60)); err != nil {
		t.Fatalf("cut text: %v", err)
	}
	time.Sleep(400 * time.Millisecond) // let a few keys through
	if err := keyEvent(tc, 'a', true); err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := keyEvent(tc, 'a', false); err != nil {
		t.Fatalf("key: %v", err)
	}
	if !log.waitFor("canceled after", 10*time.Second) {
		t.Fatalf("cancel never fired; logs: %v", log.ls)
	}
	if n := log.canceledOf(); n < 0 || n >= 60 {
		t.Fatalf("canceled after %d/60 — want mid-flight", n)
	}
	t.Logf("cancel works: stopped after %d/60 chars", log.canceledOf())

	// ── 3. single-flight: second paste during a running one is ignored ──
	if err := clientCutText(tc, strings.Repeat("b", 40)); err != nil {
		t.Fatalf("cut text: %v", err)
	}
	if err := clientCutText(tc, "second"); err != nil { // readLoop is serial: the first has claimed the slot
		t.Fatalf("cut text: %v", err)
	}
	if !log.waitFor("one already running", 5*time.Second) {
		t.Fatalf("single-flight did not reject the second paste; logs: %v", log.ls)
	}
	if err := keyEvent(tc, 'b', true); err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := keyEvent(tc, 'b', false); err != nil {
		t.Fatalf("key: %v", err)
	}
	if !log.waitFor("canceled after", 10*time.Second) {
		t.Fatalf("cancel never fired; logs: %v", log.ls)
	}
	t.Log("single-flight works: second paste rejected while first runs")

	// ── 4. visible keystroke proof with on-screen echo ──
	// F2 opens the DCUI "Authentication Required" dialog; its Login Name
	// field echoes every character. Pasting text there and eyeballing the
	// PNG verifies end to end: keys reach the host, none dropped at pace,
	// and the synthesised Shift produced the capitals. No Enter is ever
	// injected — the dialog is dismissed with ESC, nothing executes.
	//
	// Known DCUI quirks (probed on real hardware): after ESC the console
	// repaints with inverted colours (harmless), and a key sent too soon
	// after a paste cancellation gets swallowed — so this step settles
	// first and retries F2 if the dialog did not appear.
	time.Sleep(2 * time.Second)
	before, _ := snapPNG(t, fb, png("4-before-f2"))
	openDialog := func() int {
		if err := keyEvent(tc, 0xFFBF, true); err != nil { // F2
			t.Fatalf("key: %v", err)
		}
		if err := keyEvent(tc, 0xFFBF, false); err != nil {
			t.Fatalf("key: %v", err)
		}
		time.Sleep(3 * time.Second)
		img, _ := snapPNG(t, fb, png("5-f2-dialog"))
		return diffCount(before, img)
	}
	d := openDialog()
	for try := 2; d < 500 && try <= 3; try++ {
		t.Logf("F2 swallowed (try %d, diff %d) — recovering with ESC, retrying", try-1, d)
		if err := keyEvent(tc, 0xFF1B, true); err != nil {
			t.Fatalf("key: %v", err)
		}
		if err := keyEvent(tc, 0xFF1B, false); err != nil {
			t.Fatalf("key: %v", err)
		}
		time.Sleep(2 * time.Second)
		d = openDialog()
	}
	if d < 500 {
		t.Errorf("F2 opened no visible dialog after retries (diff %d px)", d)
	} else {
		t.Logf("F2 opened the DCUI dialog (diff %d px) — keystrokes reach the host", d)
	}

	// Paste into the focused Login Name field (pre-filled "root" gets
	// appended to). Every character of "Mammoth-Paste_0" must appear in
	// order — the PNG is the evidence.
	if err := clientCutText(tc, "Mammoth-Paste_0"); err != nil {
		t.Fatalf("cut text: %v", err)
	}
	if !log.waitFor("typed 15 chars", 30*time.Second) {
		t.Fatalf("echo paste never completed; logs: %v", log.ls)
	}
	time.Sleep(1 * time.Second) // DCUI redraw
	snapPNG(t, fb, png("6-echo-verify"))

	if err := keyEvent(tc, 0xFF1B, true); err != nil { // ESC — never Enter
		t.Fatalf("key: %v", err)
	}
	if err := keyEvent(tc, 0xFF1B, false); err != nil {
		t.Fatalf("key: %v", err)
	}
	time.Sleep(2 * time.Second)
	_, nEsc := snapPNG(t, fb, png("7-restored"))
	// The post-ESC repaint may show the console in inverted colours — a
	// DCUI rendering quirk, not a keyboard-path problem; report only.
	t.Logf("after ESC: nonBlack=%d (dialog closed, console back on its main screen)", nEsc)

	if err := tc.Close(); err != nil {
		t.Fatalf("close viewer: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("bridge serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not stop after viewer close")
	}
}
