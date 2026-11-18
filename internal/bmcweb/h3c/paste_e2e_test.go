package h3c

// Real-hardware E2E for the H3C paste bridge, gated by MAMMOTH_H3C_E2E:
//
//	MAMMOTH_H3C_E2E='host=192.168.1.72 pass=Geesunn@123' MAMMOTH_H3C_E2E_PNG=/tmp/pngs \
//	  go test ./internal/bmcweb/h3c/ -run TestE2EH3CBridge -v
//
// The VNC password is the credential password truncated to 8 chars, same
// rule the engine applies (kvm.go truncateRFBPassword). BridgeRFB does the
// DES dance; the fake viewer declares RAW-only support so every update
// arrives as pixels a test framebuffer can render — then the same proof
// as the Dell run: F2 opens the DCUI dialog, pasted text echoes in its
// Login Name field, ESC dismisses. No Enter is ever injected.

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func h3cE2EConfig(t *testing.T) (host, vncPass string) {
	t.Helper()
	spec := os.Getenv("MAMMOTH_H3C_E2E")
	if spec == "" {
		t.Skip("MAMMOTH_H3C_E2E not set; skipping real-HDM E2E")
	}
	var h, p string
	_, err := fmt.Sscanf(spec, "host=%s pass=%s", &h, &p)
	if err != nil || h == "" || p == "" {
		t.Fatalf("bad MAMMOTH_H3C_E2E spec: %q (%v) — want 'host=IP pass=P'", spec, err)
	}
	if len(p) > 8 {
		p = p[:8] // the RFB 8-byte key, same truncation the engine applies
	}
	return h, p
}

// rawViewer renders RAW-encoded RFB updates into an in-memory framebuffer.
// The viewer declares RAW-only support (SetEncodings [0]), so the device
// has no other pixel codec to fall back on and every rect is plain pixels.
type rawViewer struct {
	mu sync.Mutex
	fb *image.RGBA
	w  int
	h  int
}

func newRawViewer(w, h int, pf []byte) *rawViewer {
	return &rawViewer{w: w, h: h, fb: image.NewRGBA(image.Rect(0, 0, w, h))}
}

func (v *rawViewer) resize(w, h int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.w, v.h = w, h
	v.fb = image.NewRGBA(image.Rect(0, 0, w, h))
}

func (v *rawViewer) drawRect(x, y, w, h int, pf []byte, pix []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	bpp := int(pf[0]) / 8
	if bpp == 0 || x+w > v.w || y+h > v.h {
		return
	}
	redMax := binary.BigEndian.Uint16(pf[4:])
	greenMax := binary.BigEndian.Uint16(pf[6:])
	blueMax := binary.BigEndian.Uint16(pf[8:])
	redShift, greenShift, blueShift := int(pf[12]), int(pf[13]), int(pf[14])
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			o := (row*w + col) * bpp
			if o+bpp > len(pix) {
				return
			}
			var raw uint32
			switch bpp {
			case 4:
				raw = binary.BigEndian.Uint32(pix[o:])
			case 2:
				raw = uint32(binary.BigEndian.Uint16(pix[o:]))
			case 1:
				raw = uint32(pix[o])
			}
			r := uint8((raw >> redShift & uint32(redMax)) * 255 / max16(redMax))
			g := uint8((raw >> greenShift & uint32(greenMax)) * 255 / max16(greenMax))
			bv := uint8((raw >> blueShift & uint32(blueMax)) * 255 / max16(blueMax))
			v.fb.SetRGBA(x+col, y+row, color.RGBA{R: r, G: g, B: bv, A: 255})
		}
	}
}

func max16(v uint16) uint32 {
	if v == 0 {
		return 1
	}
	return uint32(v)
}

func (v *rawViewer) snapshot() *image.RGBA {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := image.NewRGBA(v.fb.Rect)
	copy(out.Pix, v.fb.Pix)
	return out
}

// viewerLoop consumes server messages forever, rendering RAW rects into
// the framebuffer; non-RAW encodings never arrive (RAW-only negotiated).
func viewerLoop(t *testing.T, c net.Conn, v *rawViewer, pf []byte) {
	readN := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(c, b); err != nil {
			t.Logf("viewer loop end: %v", err)
			return nil
		}
		return b
	}
	for {
		typ := readN(1)
		if typ == nil {
			return
		}
		switch typ[0] {
		case 0: // FramebufferUpdate
			h := readN(3)
			if h == nil {
				return
			}
			n := int(binary.BigEndian.Uint16(h[1:]))
			for i := 0; i < n; i++ {
				hdr := readN(12)
				if hdr == nil {
					return
				}
				x := int(binary.BigEndian.Uint16(hdr[0:]))
				y := int(binary.BigEndian.Uint16(hdr[2:]))
				w := int(binary.BigEndian.Uint16(hdr[4:]))
				ht := int(binary.BigEndian.Uint16(hdr[6:]))
				enc := int32(binary.BigEndian.Uint32(hdr[8:]))
				switch enc {
				case 0: // RAW
					if w == 0 || ht == 0 {
						continue
					}
					pix := readN(w * ht * int(pf[0]) / 8)
					if pix == nil {
						return
					}
					v.drawRect(x, y, w, ht, pf, pix)
				case -223: // DesktopSize
					v.resize(w, ht)
				default:
					t.Logf("viewer: unexpected encoding %d", enc)
					return
				}
			}
		case 1: // Bell — no body
		case 2: // ServerCutText
			h := readN(7)
			if h == nil {
				return
			}
			readN(int(binary.BigEndian.Uint32(h[3:])))
		default:
			t.Logf("viewer: unexpected server message %d", typ[0])
			return
		}
	}
}

func fbur(c net.Conn, incremental bool) error {
	b := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if incremental {
		b[1] = 1
	}
	_, err := c.Write(b)
	return err
}

func setRawOnly(c net.Conn) error {
	// SetEncodings: type, pad, one encoding (RAW = 0).
	_, err := c.Write([]byte{2, 0, 0, 1, 0, 0, 0, 0})
	return err
}

func TestE2EH3CBridge(t *testing.T) {
	host, vncPass := h3cE2EConfig(t)
	outDir := os.Getenv("MAMMOTH_H3C_E2E_PNG")
	if outDir == "" {
		outDir = t.TempDir()
	}
	png := func(name string) string { return filepath.Join(outDir, host+"-"+name+".png") }

	// 0. Probe the VNC service: a live RFB server greets us unprompted.
	probe, err := net.DialTimeout("tcp", host+":5900", 5*time.Second)
	if err != nil {
		t.Fatalf("dial 5900: %v", err)
	}
	_ = probe.SetReadDeadline(time.Now().Add(5 * time.Second))
	ver := make([]byte, 12)
	if _, err := io.ReadFull(probe, ver); err != nil {
		t.Fatalf("5900 sent no RFB banner (%v) — the HDM KVM service is off; enable it in the HDM web (修改服务) first", err)
	}
	probe.Close()
	t.Logf("VNC service live: %q", ver)

	// 1. BridgeRFB: the production bridge, fake viewer on the other side.
	ws, wst := net.Pipe()
	defer wst.Close()
	log := &capLog{}
	served := make(chan error, 1)
	go func() { served <- BridgeRFB(ws, host+":5900", vncPass, log) }()

	// Viewer handshake against the bridge (which fronts the device).
	bver := make([]byte, 12)
	if _, err := io.ReadFull(wst, bver); err != nil {
		t.Fatalf("bridge version: %v", err)
	}
	if _, err := wst.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	sec := make([]byte, 2)
	if _, err := io.ReadFull(wst, sec); err != nil {
		t.Fatal(err)
	}
	if sec[0] != 1 {
		t.Fatalf("offered security %v, want None", sec)
	}
	if _, err := wst.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	res := make([]byte, 4)
	if _, err := io.ReadFull(wst, res); err != nil {
		t.Fatal(err)
	}
	if _, err := wst.Write([]byte{1}); err != nil { // ClientInit
		t.Fatal(err)
	}
	si := make([]byte, 24)
	if _, err := io.ReadFull(wst, si); err != nil {
		t.Fatal(err)
	}
	w := int(binary.BigEndian.Uint16(si[0:]))
	h := int(binary.BigEndian.Uint16(si[2:]))
	nameLen := binary.BigEndian.Uint32(si[20:])
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(wst, name); err != nil {
		t.Fatal(err)
	}
	t.Logf("serverinit: %dx%d %q pixelFormat=% x", w, h, name, si[4:20])

	// 2. RAW-only viewer loop, full refresh, first snapshot.
	if err := setRawOnly(wst); err != nil {
		t.Fatal(err)
	}
	v := newRawViewer(w, h, si[4:20])
	go viewerLoop(t, wst, v, si[4:20])
	if err := fbur(wst, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	time.Sleep(2 * time.Second)
	if err := fbur(wst, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	idle := v.snapshot()
	nonBlack := 0
	for i := 0; i+3 < len(idle.Pix); i += 4 {
		if idle.Pix[i] > 8 || idle.Pix[i+1] > 8 || idle.Pix[i+2] > 8 {
			nonBlack++
		}
	}
	t.Logf("idle framebuffer: %d/%d non-black pixels", nonBlack, w*h)
	savePNG(t, idle, png("0-idle"))

	// 3. F2 opens the DCUI dialog (echo field), paste into it, ESC out.
	pressKey(wst, 0xFFBF) // F2
	time.Sleep(3 * time.Second)
	if err := fbur(wst, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	afterF2 := v.snapshot()
	savePNG(t, afterF2, png("1-f2-dialog"))

	if err := cutText(wst, "Mammoth-Paste_0"); err != nil {
		t.Fatal(err)
	}
	if !log.waitFor("typed 15 chars", 30*time.Second) {
		t.Fatalf("paste never completed; logs: %v", log.ls)
	}
	time.Sleep(2 * time.Second)
	if err := fbur(wst, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	echo := v.snapshot()
	savePNG(t, echo, png("2-echo-verify"))

	pressKey(wst, 0xFF1B) // ESC — never Enter
	time.Sleep(3 * time.Second)
	if err := fbur(wst, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	restored := v.snapshot()
	savePNG(t, restored, png("3-restored"))

	if err := wst.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		t.Logf("bridge exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop after viewer close")
	}
}

func pressKey(c net.Conn, sym uint32) {
	b := []byte{4, 1, 0, 0}
	b = binary.BigEndian.AppendUint32(b, sym)
	_, _ = c.Write(b)
	b[1] = 0
	_, _ = c.Write(b)
}

func savePNG(t *testing.T, img *image.RGBA, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("framebuffer → %s", path)
}
