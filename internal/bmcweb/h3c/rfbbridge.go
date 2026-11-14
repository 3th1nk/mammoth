// RFB passthrough bridge for H3C HDM: the HDM's VNC service (port 5900) is
// a standard RFB 3.8 server with VNC (DES challenge) authentication — real
// run 2026-11-14, docs/compat/h3c.md §KVM. The bridge dials it, performs
// the VNC authentication on behalf of the browser (the VNC password is the
// credential password, truncated to the RFB 8-byte limit), and then passes
// bytes straight through between the noVNC websocket and the device.
//
// The handshake is message-level (version, security choice, the forged
// challenge/result pair for noVNC's state machine, ClientInit/ServerInit);
// after ServerInit the two byte streams are piped verbatim — noVNC and the
// device speak the same RFB 3.8, so every pixel encoding, input message and
// pseudo-encoding flows through unchanged.

package h3c

import (
	"crypto/des"
	"fmt"
	"io"
	"net"
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

	log.Printf("avct: rfb passthrough connected (device %s)", deviceAddr)

	// ── byte passthrough both ways after the handshake ──
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(ws, dev)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(dev, ws)
		errc <- err
	}()
	return <-errc
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
