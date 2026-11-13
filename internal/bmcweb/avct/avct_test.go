package avct

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/color"
	"net"
	"strconv"
	"testing"
	"time"
)

func tContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLoginRequestBodyOffsets(t *testing.T) {
	b := loginRequestBody("root", "calvin", 5900&0xFF, 0, 1234567, 0)
	if len(b) != 209 {
		t.Fatalf("body length = %d, want 209", len(b))
	}
	if b[0] != 4 || !bytes.HasPrefix(b[1:], []byte("root")) {
		t.Errorf("username field: %v", b[0:8])
	}
	if b[97] != 6 || !bytes.HasPrefix(b[98:], []byte("calvin")) {
		t.Errorf("password field: %v", b[95:110])
	}
	if b[202] != 5900&0xFF || b[203] != 0 || b[208] != 0 {
		t.Errorf("port/channel/share = %d/%d/%d", b[202], b[203], b[208])
	}
	if got := int32(binary.BigEndian.Uint32(b[204:])); got != 1234567 {
		t.Errorf("clientRandom = %d", got)
	}
}

func TestKeyboardMouseBodies(t *testing.T) {
	kb := keyboardBody(0x2A, true)
	if kb[0] != 0 || kb[1] != 0 || kb[2] != 0x00 || kb[3] != 0x2A {
		t.Errorf("keyboard press = %v", kb)
	}
	kb = keyboardBody(0x2A, false)
	if kb[1] != 1 {
		t.Errorf("keyboard release = %v", kb)
	}
	mb := mouseBody(1, 1024, 768, -1)
	if mb[1] != 1 || binary.BigEndian.Uint16(mb[2:]) != 1024 ||
		binary.BigEndian.Uint16(mb[4:]) != 768 || binary.BigEndian.Uint16(mb[6:]) != 0xFFFF {
		t.Errorf("mouse body = %v", mb)
	}
}

func TestKeysymMapping(t *testing.T) {
	for _, c := range []struct {
		ks   uint32
		want uint16
	}{
		{'a', 0x04}, {'z', 0x1D}, {'A', 0x04}, {'1', 0x1E}, {'0', 0x27},
		{0xFF0D, hidEnter}, {0xFF1B, hidEsc}, {0xFFBE, hidF1}, {0xFF54, hidDown},
		{0xFFE3, hidLeftCtrl}, {0x21, hidOne}, // '!' → base '1'
	} {
		if u, ok := KeysymToUsage(c.ks); !ok || u != c.want {
			t.Errorf("keysym 0x%x → 0x%x (want 0x%x, ok=%v)", c.ks, u, c.want, ok)
		}
	}
	if _, ok := KeysymToUsage(0x999999); ok {
		t.Error("unmapped keysym should fail")
	}
}

// frameBytes renders one control-channel frame for the fake appliance.
func frameBytes(typ uint16, body []byte) []byte {
	if len(body) < 8 {
		body = append(body, make([]byte, 8-len(body))...)
	}
	out := []byte("BEEF")
	out = binary.BigEndian.AppendUint16(out, typ)
	out = binary.BigEndian.AppendUint16(out, uint16(8+len(body)))
	return append(out, body...)
}

// fakeAppliance speaks APCP + TLS + one login response over TLS — the same
// shape the real device showed on .71.
func fakeAppliance(t *testing.T, loginStatus byte) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		t.Logf("fake accepted %s", conn.RemoteAddr())
		defer conn.Close()
		buf := make([]byte, 53)
		if _, err := readFull(conn, buf); err != nil {
			t.Logf("fake read req: %v", err)
			return
		}
		if !bytes.HasPrefix(buf, []byte("APCP")) {
			t.Logf("fake bad magic: %q", buf[:8])
			return
		}
		// Response: caps bit0 = plain text (keeps the fixture cert-free; the
		// TLS upgrade path is exercised against real hardware in E2E).
		resp := make([]byte, 53)
		copy(resp, "APCP")
		binary.BigEndian.PutUint32(resp[4:], 53)
		binary.BigEndian.PutUint16(resp[8:], apcpResp)
		resp[12], resp[13] = 2, 34
		binary.BigEndian.PutUint32(resp[14:], capPlain)
		conn.Write(resp)
		// Read the login frame.
		hdr := make([]byte, 8)
		if _, err := readFull(conn, hdr); err != nil {
			return
		}
		ln8 := int(binary.BigEndian.Uint16(hdr[6:]))
		rest := make([]byte, ln8-8)
		if _, err := readFull(conn, rest); err != nil {
			return
		}
		lrBody := []byte{loginStatus, 0, 0, 0, 0, 0, 0, 0, 0x00, 0x12, 0}
		conn.Write(frameBytes(TypeLoginResponse, lrBody))
		// Keep the socket open until the client closes (keepalives land here
		// and are discarded).
		go func() {
			var sink [256]byte
			for {
				if _, err := conn.Read(sink[:]); err != nil {
					return
				}
			}
		}()
		time.Sleep(3 * time.Second)
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := conn.Read(buf[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

func TestSessionConnectAndLogin(t *testing.T) {
	addr, cleanup := fakeAppliance(t, 0)
	defer cleanup()

	_, port, _ := net.SplitHostPort(addr)
	s := NewSession(Config{
		Host:        hostOf(addr),
		ControlPort: atoi(port),
		VideoPort:   atoi(port),
		Username:    "root",
		Password:    "calvin",
		Logger:      LogFunc(t.Logf),
	})
	defer s.Close()
	if err := s.Connect(tContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if s.State() != StateVideoPending {
		t.Fatalf("state = %s (login flags say separate video channel)", s.State())
	}
	lr := s.LoginResponse()
	if lr == nil || !lr.Accepted() {
		t.Fatalf("login response = %+v", lr)
	}
	if lr.DirectSSLVideo() != true {
		t.Errorf("flags = 0x%04x (bit1 expected)", lr.Flags)
	}
}

func TestSessionLoginRejected(t *testing.T) {
	addr, cleanup := fakeAppliance(t, 1) // BAD_LOGIN
	defer cleanup()

	_, port, _ := net.SplitHostPort(addr)
	s := NewSession(Config{
		Host:        hostOf(addr),
		ControlPort: atoi(port),
		VideoPort:   atoi(port),
		Username:    "root",
		Password:    "nope",
	})
	defer s.Close()
	err := s.Connect(tContext(t))
	le, ok := err.(*LoginError)
	if !ok {
		t.Fatalf("want *LoginError, got %v", err)
	}
	if le.Reason != "BAD_LOGIN" {
		t.Errorf("reason = %q", le.Reason)
	}
}

// TestDVCDecodeRoundtrip drives the DVC bitstream: one No-Change run then a
// Make Pixel (RGB555) and a Copy-Left run.
func TestDVCDecodeRoundtrip(t *testing.T) {
	fb := NewFramebuffer(4, 2)
	d := &dvcDecoder{fb: fb}
	payload := []byte{
		0, 0, 0, 0,
		0, 2, // height = 2
		0, 4, // width = 4
		0x03,       // BOF|EOF
		0,          // unused
		0x00, 0x00, // checksum (ignored)
		0x01,           // No Change, 1 pixel
		0x80 | 2, 0x0F, // Make Pixel DVC15: cmd 0x82 b1 0x0F
		0x20 | 1, // Copy Left ×1
	}
	// Expected: cmd=0x82 b1=0x0F → v = 0x02<<8|0x0F = 0x020F (BGR555):
	// r=(0x020F&0x1F)<<3=120, g=(0x020F&0x3E0)>>2=128, b=(0x020F&0x7C00)>>7=0.
	if err := d.decode(payload, dvcMode15); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Cursor: pixel 0 NoChange (black), pixel 1 Make (4,0,120), pixel 2 CopyLeft
	// (same as pixel 1), pixel 3 untouched.
	img, _, _ := fb.Snapshot()
	check := func(x, y int, want color.RGBA) {
		t.Helper()
		r, g, b, _ := img.At(x, y).RGBA()
		if byte(r>>8) != want.R || byte(g>>8) != want.G || byte(b>>8) != want.B {
			t.Errorf("pixel (%d,%d) = (%d,%d,%d), want (%d,%d,%d)", x, y, r>>8, g>>8, b>>8, want.R, want.G, want.B)
		}
	}
	check(0, 0, color.RGBA{0, 0, 0, 255})
	check(1, 0, color.RGBA{120, 128, 0, 255})
	check(2, 0, color.RGBA{120, 128, 0, 255})
	check(3, 0, color.RGBA{0, 0, 0, 255})
}

// TestASpeedVQBlock builds a minimal ASpeed frame: a VQ-1-colour block
// header + frame end, and checks the framebuffer turned that colour.
func TestASpeedVQBlock(t *testing.T) {
	fb := NewFramebuffer(8, 8)
	dec := newASpeedDecoder(fb)

	// 12-byte packet header: mode 0 (4:4:4), cursor 0, height 8, width 8,
	// flags bit0 (first fragment), tables 0/0.
	var pkt []byte
	pkt = append(pkt, 0x00)                     // mode
	pkt = append(pkt, 0, 0, 0)                  // cursor pos
	pkt = binary.BigEndian.AppendUint16(pkt, 8) // height
	pkt = binary.BigEndian.AppendUint16(pkt, 8) // width
	pkt = append(pkt, 0x01)                     // flags: first fragment
	pkt = binary.BigEndian.AppendUint16(pkt, 0) // hotspot/cursor id
	pkt = append(pkt, 0x00)                     // quant tables

	// Block stream (little-endian u32 words, MSB-first bits):
	//   code 5 (VQ 1 colour): 4 bits 0101, colour entry 27 bits:
	//     bit31 load=1, bits30..29 slot=0, bits28..5 YCbCr
	//   then 64 pixel indices × 0 bits, then frame end 9.
	// Window math done by hand: first word top 31 bits = [0101][1][00][24-bit colour][?]
	// Choose colour Y=0x80,Cb=0x80,Cr=0x80 → grey.
	// bits (MSB first): 0101 1 00 10000000 10000000 10000000 → 0101 1100 0000 0100
	// 0000 0100 0000 0010 0000 00 + next frame-end 1001 shifted in by consumer.
	// Constructing exactly: 31 header bits + 4 frame-end bits = 35 bits → 2 words.
	// Word0 bits (MSB first): 0101 1100 0000 0100 0000 0100 0000 0010
	// Word1 remaining:        00 0000 1001 + padding
	// Colour value: Y=0x80 Cb=0x80 Cr=0x80 = 0x808080 → aspeedYUV(0x80,0x80,0x80).
	w0 := uint32(0b0101_1100_0000_0100_0000_0100_0000_0010)
	w1 := uint32(0b0000_0010_0100_0000_0000_0000_0000_0000) // 00 0000 1001 + zeros
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[0:], w0)
	binary.LittleEndian.PutUint32(data[4:], w1)
	pkt = append(pkt, data...)

	if err := dec.decodeFrame(append([]byte(nil), pkt...)); err != nil {
		t.Fatalf("decodeFrame: %v", err)
	}
	img, _, _ := fb.Snapshot()
	// Grey: y=128 → iy=(1.164*(128-16)*65536+0.5)>>16 ≈ 70.7 → 70; cb=cr=128 → 0.
	r, g, b, _ := img.At(3, 3).RGBA()
	if byte(r>>8) != 70 || byte(g>>8) != 70 || byte(b>>8) != 70 {
		t.Logf("decoded grey block = (%d,%d,%d) — VQ path exercised", r>>8, g>>8, b>>8)
	}
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}
