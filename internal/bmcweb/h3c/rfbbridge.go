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
// unknown message type tears the session down — the RFB client message set
// is small and closed, and after a desync the byte stream is meaningless.
func clientToDevice(ws, dev net.Conn, devMu *sync.Mutex, paste *pasteState) error {
	var typ [1]byte
	for {
		if _, err := io.ReadFull(ws, typ[:]); err != nil {
			return err
		}
		switch typ[0] {
		case 0: // SetPixelFormat — 19 bytes follow
			b := make([]byte, 19)
			if _, err := io.ReadFull(ws, b); err != nil {
				return err
			}
			if err := deviceWrite(dev, devMu, append([]byte{0}, b...)); err != nil {
				return err
			}
		case 2: // SetEncodings
			var hb [3]byte
			if _, err := io.ReadFull(ws, hb[:]); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(hb[1:]))
			enc := make([]byte, n*4)
			if _, err := io.ReadFull(ws, enc); err != nil {
				return err
			}
			msg := append([]byte{2}, hb[1:]...)
			if err := deviceWrite(dev, devMu, append(msg, enc...)); err != nil {
				return err
			}
		case 3: // FramebufferUpdateRequest — 9 bytes follow
			b := make([]byte, 9)
			if _, err := io.ReadFull(ws, b); err != nil {
				return err
			}
			if err := deviceWrite(dev, devMu, append([]byte{3}, b...)); err != nil {
				return err
			}
		case 4: // KeyEvent — a real key press also cancels a running paste
			b := make([]byte, 7)
			if _, err := io.ReadFull(ws, b); err != nil {
				return err
			}
			if b[0] != 0 {
				paste.cancelNow()
			}
			if err := deviceWrite(dev, devMu, append([]byte{4}, b...)); err != nil {
				return err
			}
		case 5: // PointerEvent — 5 bytes follow
			b := make([]byte, 5)
			if _, err := io.ReadFull(ws, b); err != nil {
				return err
			}
			if err := deviceWrite(dev, devMu, append([]byte{5}, b...)); err != nil {
				return err
			}
		case 6: // ClientCutText — intercepted, never forwarded
			var hb [7]byte
			if _, err := io.ReadFull(ws, hb[:]); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(hb[3:])
			if n > 1<<20 {
				return fmt.Errorf("rfb: cut text too large")
			}
			text := make([]byte, n)
			if _, err := io.ReadFull(ws, text); err != nil {
				return err
			}
			paste.start(text)
		default:
			return fmt.Errorf("rfb: unknown client message type %d", typ[0])
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
