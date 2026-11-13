package avct

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"sync"
	"time"
)

// RFBBridge is an RFB 3.8 server speaking to one viewer (noVNC over the
// engine's websocket, surfaced as a net.Conn) backed by a console
// Framebuffer. Updates are RAW rectangles in the client's pixel format —
// noVNC negotiates 32bpp and the codec-side work (JPEG/DVC) is already done,
// so no remote-encoding machinery is needed.
//
// Input flows the other way: RFB KeyEvent keysyms map to USB HID usages
// (keymap.go) and PointerEvent absolute coordinates ride the appliance's
// mouse packets directly.

type RFBBridge struct {
	FB     *Framebuffer
	Input  InputSink // nil = view-only
	Name   string
	Logger Logger
}

// InputSink receives RFB client events.
type InputSink interface {
	KeyDown(usage uint16) error
	KeyUp(usage uint16) error
	Pointer(buttons uint8, x, y, wheel int) error
}

// RFB button mask bits (wire order: left, middle, right, wheel up/down).
const (
	rfbLeft      = 1 << 0
	rfbMiddle    = 1 << 1
	rfbRight     = 1 << 2
	rfbWheelUp   = 1 << 3
	rfbWheelDown = 1 << 4
)

// serverPixelFormat: 32bpp RGBA, shifts as the browser's noVNC negotiates.
type rfbPixelFormat struct {
	BPP, Depth, BigEndian, TrueColour uint8
	RedMax, GreenMax, BlueMax         uint16
	RedShift, GreenShift, BlueShift   uint8
}

var rfbServerPF = rfbPixelFormat{BPP: 32, Depth: 24, TrueColour: 1, RedMax: 255, GreenMax: 255, BlueMax: 255, RedShift: 16, GreenShift: 8, BlueShift: 0}

func (p rfbPixelFormat) encode() []byte {
	b := make([]byte, 16)
	b[0], b[1], b[2], b[3] = p.BPP, p.Depth, p.BigEndian, p.TrueColour
	binary.BigEndian.PutUint16(b[4:], p.RedMax)
	binary.BigEndian.PutUint16(b[6:], p.GreenMax)
	binary.BigEndian.PutUint16(b[8:], p.BlueMax)
	b[10], b[11], b[12] = p.RedShift, p.GreenShift, p.BlueShift
	return b
}

const (
	rfbEncRaw     = 0
	rfbEncDesktop = -223
)

// Serve runs the RFB session over conn until either side drops it.
func (b *RFBBridge) Serve(conn net.Conn) error {
	log := b.Logger
	if log == nil {
		log = nopLogger{}
	}
	c := &rfbClient{
		bridge: b, conn: conn,
		rd: bufio.NewReaderSize(conn, 64<<10),
		wr: bufio.NewWriterSize(conn, 256<<10),
		pf: rfbServerPF,
	}
	defer conn.Close()
	if err := c.handshake(); err != nil {
		return fmt.Errorf("rfb: handshake: %w", err)
	}
	log.Printf("avct: rfb viewer connected")

	cctx := conn // deadline-based stop channel is via Close on the conn
	go c.updateLoop(cctx)
	err := c.readLoop()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	log.Printf("avct: rfb viewer disconnected")
	return nil
}

type rfbClient struct {
	bridge *RFBBridge
	conn   net.Conn
	rd     *bufio.Reader
	wr     *bufio.Writer

	wmu sync.Mutex
	pf  rfbPixelFormat
	enc map[int32]bool

	w, h   int
	dirty  image.Rectangle
	wantUp bool
	dmu    sync.Mutex
	notify chan struct{}

	wheelMask uint8
}

// handshake: RFB 3.8 with the None security type (the websocket + gate
// cookie are the auth boundary), then the geometry/pixel-format exchange.
func (c *rfbClient) handshake() error {
	_ = c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	if _, err := c.conn.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(c.rd, ver); err != nil {
		return err
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return fmt.Errorf("unsupported client version %q", ver)
	}
	if minor < 7 {
		// RFB 3.3: the server picks None unconditionally.
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], 1)
		if _, err := c.conn.Write(b[:]); err != nil {
			return err
		}
	} else {
		// RFB 3.7+: one type offered, client echoes it.
		if _, err := c.conn.Write([]byte{1, 1}); err != nil {
			return err
		}
		var chosen [1]byte
		if _, err := io.ReadFull(c.rd, chosen[:]); err != nil {
			return err
		}
		if chosen[0] != 1 {
			return fmt.Errorf("client chose security type %d", chosen[0])
		}
		var res [4]byte // security result OK (3.8 requires it; harmless for 3.7)
		if _, err := c.conn.Write(res[:]); err != nil {
			return err
		}
	}
	// ClientInit (shared flag ignored: the console session is the appliance's).
	if _, err := c.rd.ReadByte(); err != nil {
		return err
	}

	w, h := c.bridge.FB.Size()
	if w == 0 || h == 0 {
		w, h = 1024, 768
	}
	name := c.bridge.Name
	if name == "" {
		name = "console"
	}
	var init bytes.Buffer
	init.Write(binary.BigEndian.AppendUint16(nil, uint16(w)))
	init.Write(binary.BigEndian.AppendUint16(nil, uint16(h)))
	init.Write(rfbServerPF.encode())
	init.Write(binary.BigEndian.AppendUint32(nil, uint32(len(name))))
	init.WriteString(name)
	if _, err := c.conn.Write(init.Bytes()); err != nil {
		return err
	}
	c.w, c.h = w, h
	c.enc = map[int32]bool{rfbEncRaw: true}
	c.notify = make(chan struct{}, 1)
	return nil
}

// readLoop dispatches client messages.
func (c *rfbClient) readLoop() error {
	for {
		t, err := c.rd.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0: // SetPixelFormat — read and deliberately ignore: the RFB spec
			// lets the server keep its format, and noVNC renders whatever
			// ServerInit declared (we always send 32bpp RGBA). Honouring
			// arbitrary client formats would multiply the translate paths
			// for no visual benefit.
			var b [19]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return err
			}
			c.markAllDirty()
		case 2: // SetEncodings
			var b [3]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(b[1:]))
			enc := map[int32]bool{rfbEncRaw: true}
			for i := 0; i < n; i++ {
				var e int32
				if err := binary.Read(c.rd, binary.BigEndian, &e); err != nil {
					return err
				}
				enc[e] = true
			}
			c.wmu.Lock()
			c.enc = enc
			c.wmu.Unlock()
		case 3: // FramebufferUpdateRequest
			var b [9]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return err
			}
			c.dmu.Lock()
			if b[0] == 0 { // non-incremental: full refresh
				w, h := c.bridge.FB.Size()
				c.dirty = image.Rect(0, 0, w, h)
			}
			c.wantUp = true
			c.dmu.Unlock()
			select {
			case c.notify <- struct{}{}:
			default:
			}
		case 4: // KeyEvent
			var b [7]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return err
			}
			if c.bridge.Input == nil {
				continue
			}
			keysym := binary.BigEndian.Uint32(b[3:])
			usage, ok := KeysymToUsage(keysym)
			if !ok {
				continue // unmapped keys: skip silently (macOS sends odd extras)
			}
			var err error
			if b[0] != 0 {
				err = c.bridge.Input.KeyDown(usage)
			} else {
				err = c.bridge.Input.KeyUp(usage)
			}
			if err != nil {
				c.bridge.Logger.Printf("avct: key 0x%x: %v", keysym, err)
			}
		case 5: // PointerEvent
			var b [5]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return err
			}
			if c.bridge.Input == nil {
				continue
			}
			mask := b[0]
			x := int(binary.BigEndian.Uint16(b[1:]))
			y := int(binary.BigEndian.Uint16(b[3:]))
			var buttons uint8
			if mask&rfbLeft != 0 {
				buttons |= 1
			}
			if mask&rfbMiddle != 0 {
				buttons |= 2
			}
			if mask&rfbRight != 0 {
				buttons |= 4
			}
			// Wheel: report on the press edge only.
			wheel := 0
			if mask&rfbWheelUp != 0 && c.wheelMask&rfbWheelUp == 0 {
				wheel = 1
			}
			if mask&rfbWheelDown != 0 && c.wheelMask&rfbWheelDown == 0 {
				wheel = -1
			}
			c.wheelMask = mask & (rfbWheelUp | rfbWheelDown)
			if err := c.bridge.Input.Pointer(buttons, x, y, wheel); err != nil {
				c.bridge.Logger.Printf("avct: pointer: %v", err)
			}
		case 6: // ClientCutText
			var b [7]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(b[3:])
			if n > 1<<20 {
				return errors.New("cut text too large")
			}
			if _, err := io.CopyN(io.Discard, c.rd, int64(n)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown rfb message type %d", t)
		}
	}
}

func (c *rfbClient) markAllDirty() {
	w, h := c.bridge.FB.Size()
	c.dmu.Lock()
	c.dirty = image.Rect(0, 0, w, h)
	c.dmu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// updateLoop coalesces decoder damage into RFB updates (40ms cadence).
func (c *rfbClient) updateLoop(_ net.Conn) {
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.notify:
		case <-t.C:
		}
		// Pull the decoder's damage into our view: the framebuffer
		// accumulates it per decoded frame, we coalesce per update cycle.
		d := c.bridge.FB.Drain()
		c.dmu.Lock()
		if !d.Empty() {
			c.dirty = c.dirty.Union(d)
		}
		want := c.wantUp
		if !want {
			c.dmu.Unlock()
			continue
		}
		w, h := c.bridge.FB.Size()
		resized := w != c.w || h != c.h
		dirty := c.dirty
		if resized {
			dirty = image.Rect(0, 0, w, h)
		}
		if dirty.Empty() && !resized {
			c.dmu.Unlock()
			continue
		}
		c.dirty = image.Rectangle{}
		c.wantUp = false
		c.dmu.Unlock()
		if err := c.sendUpdate(dirty, resized, w, h); err != nil {
			_ = c.conn.Close()
			return
		}
	}
}

// sendUpdate renders the dirty region (plus a DesktopSize pseudo-rect when
// the resolution changed) in the client's pixel format.
func (c *rfbClient) sendUpdate(dirty image.Rectangle, resized bool, w, h int) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var rects []([]byte)
	if resized {
		c.w, c.h = w, h
		hdr := make([]byte, 12)
		binary.BigEndian.PutUint16(hdr[4:], uint16(w))
		binary.BigEndian.PutUint16(hdr[6:], uint16(h))
		binary.BigEndian.PutUint32(hdr[8:], 0xFFFFFF21) // encoding -223 (DesktopSize)
		rects = append(rects, hdr)
		dirty = image.Rect(0, 0, w, h)
	}
	if !dirty.Empty() {
		dirty = dirty.Intersect(image.Rect(0, 0, w, h)).Intersect(image.Rect(0, 0, c.w, c.h))
		if !dirty.Empty() {
			img, _, _ := c.bridge.FB.Snapshot()
			sub := img.SubImage(dirty).(*image.RGBA)
			hdr := make([]byte, 12)
			binary.BigEndian.PutUint16(hdr[0:], uint16(dirty.Min.X))
			binary.BigEndian.PutUint16(hdr[2:], uint16(dirty.Min.Y))
			binary.BigEndian.PutUint16(hdr[4:], uint16(dirty.Dx()))
			binary.BigEndian.PutUint16(hdr[6:], uint16(dirty.Dy()))
			binary.BigEndian.PutUint32(hdr[8:], 0) // encoding 0 = RAW
			rects = append(rects, append(hdr, c.translate(sub)...))
		}
	}
	if len(rects) == 0 {
		return nil
	}
	head := []byte{0, 0}
	head = binary.BigEndian.AppendUint16(head, uint16(len(rects)))
	if _, err := c.wr.Write(head); err != nil {
		return err
	}
	for _, r := range rects {
		if _, err := c.wr.Write(r); err != nil {
			return err
		}
	}
	return c.wr.Flush()
}

// translate converts RGBA pixels into the client's pixel format.
func (c *rfbClient) translate(img *image.RGBA) []byte {
	pf := c.pf
	w, h := img.Rect.Dx(), img.Rect.Dy()
	bpp := int(pf.BPP) / 8
	out := make([]byte, w*h*bpp)
	o := 0
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			r, g, b := uint32(row[x*4]), uint32(row[x*4+1]), uint32(row[x*4+2])
			v := (r*uint32(pf.RedMax)/255)<<pf.RedShift |
				(g*uint32(pf.GreenMax)/255)<<pf.GreenShift |
				(b*uint32(pf.BlueMax)/255)<<pf.BlueShift
			switch bpp {
			case 4:
				if pf.BigEndian != 0 {
					binary.BigEndian.PutUint32(out[o:], v)
				} else {
					binary.LittleEndian.PutUint32(out[o:], v)
				}
			case 2:
				if pf.BigEndian != 0 {
					binary.BigEndian.PutUint16(out[o:], uint16(v))
				} else {
					binary.LittleEndian.PutUint16(out[o:], uint16(v))
				}
			case 1:
				out[o] = byte(v)
			}
			o += bpp
		}
	}
	return out
}
