// RFB passthrough bridge for H3C HDM: the HDM's VNC service (port 5900) is
// a standard RFB 3.8 server with VNC (DES challenge) authentication — real
// run 2026-11-14, docs/compat/h3c.md §KVM. The bridge dials it, performs
// the VNC authentication on behalf of the browser (the VNC password is the
// credential password, truncated to the RFB 8-byte limit), and then pipes
// the two directions differently: device → client verbatim, client →
// device through a message filter (paste.go) that turns ClientCutText into
// typed keystrokes — the HDM has no cut-text consumer, dead bytes would go
// nowhere. Everything else (pixel encodings, input, pseudo-encodings)
// flows through unchanged.

package h3c

import (
	"crypto/des"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// rfbDESKey derives the RFB DES key: password truncated/padded to 8 bytes,
// each byte bit-reversed (RFB 6.5.2). The HDM limits the VNC password to
// 8 characters (settings page), so no truncation surprises.
func rfbDESKey(password string) []byte {
	key := make([]byte, 8)
	copy(key, []byte(password))
	for i := range key {
		b := key[i]
		var r byte
		for j := 0; j < 8; j++ {
			r = r<<1 | b&1
			b >>= 1
		}
		key[i] = r
	}
	return key
}

// rfbVNCResponse computes the standard challenge-response.
func rfbVNCResponse(password string, challenge []byte) []byte {
	c, err := des.NewCipher(rfbDESKey(password))
	if err != nil {
		return nil
	}
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out
}

// BridgeRFB serves one noVNC client (the websocket surfaced as net.Conn by
// the caller, binary mode) against the HDM's RFB server.
func BridgeRFB(ws net.Conn, deviceAddr, vncPassword string, log interface {
	Printf(format string, args ...any)
}) error {
	deadline := time.Now().Add(20 * time.Second)

	dev, err := net.Dial("tcp", deviceAddr)
	if err != nil {
		return fmt.Errorf("rfb: dial %s: %w", deviceAddr, err)
	}
	defer dev.Close()
	_ = dev.SetDeadline(deadline)

	// ── ProtocolVersion: device → client → back ──
	devVer := make([]byte, 12)
	if _, err := io.ReadFull(dev, devVer); err != nil {
		return fmt.Errorf("rfb: device version: %w", err)
	}
	if _, err := ws.Write(devVer); err != nil {
		return err
	}
	cliVer := make([]byte, 12)
	if _, err := io.ReadFull(ws, cliVer); err != nil {
		return err
	}
	if _, err := dev.Write(cliVer); err != nil {
		return err
	}

	// ── SecurityTypes: the device offers VNC auth (type 2), but noVNC is
	// told "None" — the engine holds the credential and answers the DES
	// challenge itself, so the browser never sees (or needs) the VNC
	// password. The 3.8 SecurityResult is synthesized as OK for the client.
	types, err := wsReadCounted(dev) // 3.8: u8 count + types
	if err != nil {
		return err
	}
	hasVNC := false
	for _, t := range types[1:] {
		if t == 2 {
			hasVNC = true
		}
	}
	if _, err := ws.Write([]byte{1, 1}); err != nil { // lie: None only
		return err
	}
	choice := make([]byte, 1)
	if _, err := io.ReadFull(ws, choice); err != nil { // client echoes 1
		return err
	}
	if _, err := dev.Write([]byte{2}); err != nil { // engine picks VNC auth
		return err
	}

	var result []byte
	if hasVNC {
		chal := make([]byte, 16)
		if _, err := io.ReadFull(dev, chal); err != nil {
			return err
		}
		if _, err := dev.Write(rfbVNCResponse(vncPassword, chal)); err != nil {
			return err
		}
		result = make([]byte, 4)
		if _, err := io.ReadFull(dev, result); err != nil {
			return err
		}
		if result[0] != 0 || result[1] != 0 || result[2] != 0 || result[3] != 0 {
			return fmt.Errorf("rfb: device rejected the VNC credential (security result %x)", result)
		}
	}
	if _, err := ws.Write([]byte{0, 0, 0, 0}); err != nil { // SecurityResult OK
		return err
	}
	_ = deadline
	_ = dev.SetDeadline(time.Time{})

	// ── ClientInit (1B client → device), ServerInit (20B head + 4B name
	// length + name — verbatim to noVNC, which reads the same layout) ──
	init := make([]byte, 1)
	if _, err := io.ReadFull(ws, init); err != nil {
		return err
	}
	if _, err := dev.Write(init); err != nil {
		return err
	}
	siHead := make([]byte, 20) // width2 + height2 + pixelFormat16
	if _, err := io.ReadFull(dev, siHead); err != nil {
		return err
	}
	if _, err := ws.Write(siHead); err != nil {
		return err
	}
	nameLen := make([]byte, 4)
	if _, err := io.ReadFull(dev, nameLen); err != nil {
		return err
	}
	if _, err := ws.Write(nameLen); err != nil {
		return err
	}
	n := int(uint32(nameLen[0])<<24 | uint32(nameLen[1])<<16 | uint32(nameLen[2])<<8 | uint32(nameLen[3]))
	if n < 0 || n > 4096 {
		return fmt.Errorf("rfb: server name length %d out of range", n)
	}
	name := make([]byte, n)
	if _, err := io.ReadFull(dev, name); err != nil {
		return err
	}
	if _, err := ws.Write(name); err != nil {
		return err
	}

	log.Printf("rfb passthrough connected (device %s)", deviceAddr)

	// ── filtered passthrough: device → client stays verbatim; client →
	// device goes through the message filter so a ClientCutText becomes
	// typed keystrokes (paste.go) instead of dead bytes the HDM drops ──
	devMu := new(sync.Mutex)
	paste := &pasteState{dev: dev, devMu: devMu, log: log}
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(ws, dev)
		errc <- err
	}()
	go func() {
		errc <- clientToDevice(ws, dev, devMu, paste)
	}()
	return <-errc
}

// clientToDevice reads whole client messages from the viewer and forwards
// them verbatim, except ClientCutText: that becomes a paste injection. An
// unknown message type tears the session down — after a desync the byte
// stream is uninterpretable garbage.
//
// The set is everything real viewers send once extensions are negotiated,
// not just the six core RFB messages: noVNC requests QEMUExtendedKeyEvent
// (-261) in SetEncodings and AMI HDMs acknowledge it — from then on every
// keystroke rides message type 255, NOT plain KeyEvent. Passing 255
// through verbatim is what the pre-filter passthrough did implicitly; a
// filter that doesn't know it kills the session on the operator's first
// key press.
func clientToDevice(ws, dev net.Conn, devMu *sync.Mutex, paste *pasteState) error {
	readN := func(n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(ws, b); err != nil {
			return nil, err
		}
		return b, nil
	}
	var typ [1]byte
	for {
		if _, err := io.ReadFull(ws, typ[:]); err != nil {
			return err
		}
		var msg []byte
		cancelPaste := false
		switch typ[0] {
		case 0: // SetPixelFormat — pad(3) + pixel format(16)
			b, err := readN(19)
			if err != nil {
				return err
			}
			msg = append([]byte{0}, b...)
		case 2: // SetEncodings — pad(1) + count(2) + encodings(4n)
			hb, err := readN(3)
			if err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(hb[1:]))
			enc, err := readN(n * 4)
			if err != nil {
				return err
			}
			msg = append(append([]byte{2}, hb...), enc...)
		case 3: // FramebufferUpdateRequest — 9 bytes
			b, err := readN(9)
			if err != nil {
				return err
			}
			msg = append([]byte{3}, b...)
		case 4: // KeyEvent — a real key press also cancels a running paste
			b, err := readN(7)
			if err != nil {
				return err
			}
			cancelPaste = b[0] != 0
			msg = append([]byte{4}, b...)
		case 5: // PointerEvent — 5 bytes
			b, err := readN(5)
			if err != nil {
				return err
			}
			msg = append([]byte{5}, b...)
		case 6: // ClientCutText — intercepted, never forwarded
			hb, err := readN(7)
			if err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(hb[3:])
			if n > 1<<20 {
				return fmt.Errorf("rfb: cut text too large")
			}
			text, err := readN(int(n))
			if err != nil {
				return err
			}
			paste.start(text)
			continue
		case 150: // EnableContinuousUpdates — enable(1) + rect(8)
			b, err := readN(12)
			if err != nil {
				return err
			}
			msg = append([]byte{150}, b...)
		case 248: // ClientFence — pad(3) + flags(4) + length(1) + payload(n)
			b, err := readN(8)
			if err != nil {
				return err
			}
			payload, err := readN(int(b[7]))
			if err != nil {
				return err
			}
			msg = append(append([]byte{248}, b...), payload...)
		case 250: // xvp — version(1) + op(1)
			b, err := readN(2)
			if err != nil {
				return err
			}
			msg = append([]byte{250}, b...)
		case 251: // SetDesktopSize — pad(1) + width(2) + height(2) + screens(1) + pad(1) + screens(n·16)
			hb, err := readN(6)
			if err != nil {
				return err
			}
			screens, err := readN(int(hb[4]) * 16)
			if err != nil {
				return err
			}
			msg = append(append([]byte{251}, hb...), screens...)
		case 255: // QEMU extended — sub(1); 0=keyboard: down(2)+keysym(4)+keycode(4); 1=pointer: pad(2)+mask(1)+x(2)+y(2)
			sub, err := readN(1)
			if err != nil {
				return err
			}
			switch sub[0] {
			case 0:
				b, err := readN(10)
				if err != nil {
					return err
				}
				cancelPaste = binary.BigEndian.Uint16(b[0:2]) != 0
				msg = append([]byte{255, 0}, b...)
			case 1:
				b, err := readN(7)
				if err != nil {
					return err
				}
				msg = append([]byte{255, 1}, b...)
			default:
				return fmt.Errorf("rfb: unknown QEMU extended sub message %d", sub[0])
			}
		default:
			return fmt.Errorf("rfb: unknown client message type %d", typ[0])
		}
		if cancelPaste {
			paste.cancelNow()
		}
		if err := deviceWrite(dev, devMu, msg); err != nil {
			return err
		}
	}
}

// deviceWrite sends one whole message under the lock the injector shares —
// RFB messages must not interleave on the device connection.
func deviceWrite(dev net.Conn, mu *sync.Mutex, b []byte) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := dev.Write(b)
	return err
}

// wsReadCounted reads one u8 count followed by that many bytes (RFB
// security-type list shape).
func wsReadCounted(dev net.Conn) ([]byte, error) {
	n := make([]byte, 1)
	if _, err := io.ReadFull(dev, n); err != nil {
		return nil, err
	}
	out := append([]byte{}, n...)
	if n[0] > 0 {
		body := make([]byte, n[0])
		if _, err := io.ReadFull(dev, body); err != nil {
			return nil, err
		}
		out = append(out, body...)
	}
	return out, nil
}
