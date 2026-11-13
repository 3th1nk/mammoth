// Package avct speaks the Avocent virtual-console protocol carried by Dell
// iDRAC 6/7/8 on port 5900 (the viewer.jnlp applet's wire protocol), so the
// mammoth engine can bridge a browser noVNC client to a real KVM session.
//
// The wire facts here were verified against a real iDRAC7 2.65
// (PowerEdge R620) and documented in docs/compat/dell.md; the protocol
// itself predates Redfish and predates Dell (Avocent lineage).
//
// Layering:
//
//	transport.go — APCP pre-handshake, TLS upgrade, AVSP framing
//	session.go   — control-channel state machine (login → video → running)
//	packets.go   — packet encode/decode for the types we exchange
//	input.go     — RFB events → avct keyboard/mouse packets
//	video.go     — video-channel reader + framebuffer maintenance
//	dvc.go       — DVC15/7/23 bitstream decoder (lossless updates)
//	aspeed.go    — ASpeed JPEG decoder (iDRAC7+ full-frame updates)
//	textmode.go  — text-mode video (BIOS POST) with font/palette packets
//	keymap.go    — X11 keysym → USB HID usage mapping
//	rfb.go       — RFB 3.8 server facing the browser's noVNC client
package avct

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// Protocol version advertised in the APCP request (the iDRAC7/8 viewer
// speaks 2.34; the device echoes at least this).
const (
	protoMajor = 2
	protoMinor = 34
)

const (
	apcpMagic     = "APCP"
	apcpMsgLen    = 53 // both request and response bodies
	apcpReq       = 0x0100
	apcpResp      = 0x8100
	apcpRandomLen = 32
	// Request types: 3 = control (keyboard/mouse), 4 = video.
	APCPTypeControl = 3
	APCPTypeVideo   = 4
	// Capability bits we offer and parse: the device picks one.
	capPlain = 1 // stay clear-text
	capSSL   = 4 // upgrade this TCP connection to TLS
	capBoth  = capPlain | capSSL
)

// APCPResult is what the pre-handshake negotiated.
type APCPResult struct {
	TLS      bool
	Version  [2]byte
	Redirect int // non-zero: reconnect to this port instead (unseen on iDRAC)
}

// DialAPCP opens host:port, speaks the 53-byte APCP pre-handshake for the
// given request type, and upgrades to TLS when the device says so. The
// returned connection is ready for AVSP framing.
//
// Wire (all big-endian), request then response:
//
//	0  "APCP"            0  "APCP"
//	4  int32 53           4  int32 length
//	8  int16 0x0100      8  int16 0x8100
//	10 int16 0           10 int16 (unused)
//	12 byte type         12 byte versionMajor
//	13 byte verMajor     13 byte versionMinor
//	14 byte verMinor     14 int32 capabilities (0=refused, &1 plain, &4 SSL)
//	15 byte 0            18 int16 redirectPort (0 = keep socket)
//	16 int32 caps        20 byte randomLen
//	20 byte randomLen    21 32 bytes random
//	21 32 bytes random
func DialAPCP(ctx context.Context, host string, port int, reqType byte, timeout time.Duration, tlsCfg *tls.Config) (net.Conn, *APCPResult, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, nil, fmt.Errorf("avct: dial %s: %w", addr(host, port), err)
	}
	tcp := conn.(*net.TCPConn)
	_ = tcp.SetNoDelay(true)
	_ = tcp.SetReadBuffer(1 << 15)
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	rnd := make([]byte, apcpRandomLen)
	_, _ = rand.Read(rnd)
	req := make([]byte, 0, apcpMsgLen)
	req = append(req, apcpMagic...)
	req = binary.BigEndian.AppendUint32(req, apcpMsgLen)
	req = binary.BigEndian.AppendUint16(req, apcpReq)
	req = binary.BigEndian.AppendUint16(req, 0)
	req = append(req, reqType, protoMajor, protoMinor, 0)
	req = binary.BigEndian.AppendUint32(req, capBoth)
	req = append(req, apcpRandomLen)
	req = append(req, rnd...)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("avct: apcp write: %w", err)
	}

	resp := make([]byte, apcpMsgLen)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("avct: apcp read: %w", err)
	}
	if string(resp[:4]) != apcpMagic {
		conn.Close()
		return nil, nil, fmt.Errorf("avct: apcp bad magic %q", resp[:4])
	}
	msgType := binary.BigEndian.Uint16(resp[8:10])
	if msgType != apcpResp {
		conn.Close()
		return nil, nil, fmt.Errorf("avct: apcp unexpected type 0x%04x", msgType)
	}
	res := &APCPResult{
		Version:  [2]byte{resp[12], resp[13]},
		Redirect: int(binary.BigEndian.Uint16(resp[18:20])),
	}
	caps := binary.BigEndian.Uint32(resp[14:18])
	if caps == 0 {
		conn.Close()
		return nil, nil, errors.New("avct: apcp refused (capabilities=0)")
	}
	switch {
	case caps&capPlain != 0:
	case caps&capSSL != 0:
		cfg := tlsCfg
		if cfg == nil {
			cfg = DefaultTLSConfig()
		} else {
			cfg = cfg.Clone()
		}
		tc := tls.Client(conn, cfg)
		hctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := tc.HandshakeContext(hctx); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("avct: tls upgrade: %w", err)
		}
		conn = tc
		res.TLS = true
	default:
		conn.Close()
		return nil, nil, fmt.Errorf("avct: apcp unsupported caps %d", caps)
	}
	if res.Redirect != 0 {
		// Unseen on iDRAC (both real units answer 0), but the viewer does
		// reconnect without a second APCP exchange.
		conn.Close()
		conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(res.Redirect)))
		if err != nil {
			return nil, nil, fmt.Errorf("avct: redirect dial %d: %w", res.Redirect, err)
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		if res.TLS {
			cfg := tlsCfg
			if cfg == nil {
				cfg = DefaultTLSConfig()
			} else {
				cfg = cfg.Clone()
			}
			tc := tls.Client(conn, cfg)
			hctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := tc.HandshakeContext(hctx); err != nil {
				conn.Close()
				return nil, nil, fmt.Errorf("avct: redirect tls: %w", err)
			}
			conn = tc
		}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, res, nil
}

// DefaultTLSConfig tolerates the ancient TLS stacks in iDRAC6/7/8:
// self-signed certs, TLS 1.0 floor, and explicit legacy cipher suites (Go
// defaults no longer offer RSA key exchange / 3DES).
func DefaultTLSConfig() *tls.Config {
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		suites = append(suites, s.ID)
	}
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // iDRAC certificates are self-signed
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS12,
		CipherSuites:       suites,
	}
}

// ── AVSP framing ────────────────────────────────────────────────────────────

// Every packet on the control and video sockets after APCP carries an
// 8-byte header. Control channel: "BEEF" + type u16be + length u16be (total,
// including header). Video channel (client → server): four zero bytes + 0x01
// + type byte + length u16be. The reader picks the type field per channel:
// u16 at [4:6] on control, byte 5 on video.
const (
	frameHeaderLen = 8
	minFrameLen    = 8
	maxFrameLen    = 32768
	// The device's own reader rejects frames shorter than 16 bytes; outgoing
	// bodies are zero-padded to reach it.
	minSendFrameLen = 16
)

// Frame is one raw AVSP packet.
type Frame struct {
	Type uint16
	Body []byte
}

// Len returns the total wire length (header + body).
func (f *Frame) Len() int { return frameHeaderLen + len(f.Body) }

// Bytes renders the frame for the control channel ("BEEF" header).
func (f *Frame) Bytes() []byte {
	out := make([]byte, 0, f.Len())
	out = append(out, 'B', 'E', 'E', 'F')
	out = binary.BigEndian.AppendUint16(out, f.Type)
	out = binary.BigEndian.AppendUint16(out, uint16(f.Len()))
	return append(out, f.Body...)
}

// videoBytes renders the frame with the video-channel client header
// (00 00 00 00 01 type len16).
func (f *Frame) videoBytes() []byte {
	out := make([]byte, 0, f.Len())
	out = append(out, 0, 0, 0, 0, 1, byte(f.Type))
	out = binary.BigEndian.AppendUint16(out, uint16(f.Len()))
	return append(out, f.Body...)
}

// controlFrame builds a control-channel frame, padding the body so the
// device's reader accepts it.
func controlFrame(typ uint16, body []byte) *Frame {
	if len(body) < minSendFrameLen-frameHeaderLen {
		p := make([]byte, minSendFrameLen-frameHeaderLen)
		copy(p, body)
		body = p
	}
	return &Frame{Type: typ, Body: body}
}

// videoFrame builds a video-channel client frame.
func videoFrame(typ byte, body []byte) *Frame {
	if len(body) < minSendFrameLen-frameHeaderLen {
		p := make([]byte, minSendFrameLen-frameHeaderLen)
		copy(p, body)
		body = p
	}
	return &Frame{Type: uint16(typ), Body: body}
}

// ReadFrame reads one frame. video=true selects the video-channel framing —
// both directions there use the plain header (server: 00 00 00 00 00 type
// len16, client: 00 00 00 00 01 type len16) with NO magic, while the control
// channel carries "BEEF" and can interleave foreign magics
// ("APCP"/"ASCP"/"MGMT"/"CSCO") that are skipped by their own length field.
func ReadFrame(r io.Reader, video bool) (*Frame, error) {
	hdr := make([]byte, frameHeaderLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	var typ uint16
	var n int
	if video {
		typ = uint16(hdr[5])
		n = int(binary.BigEndian.Uint16(hdr[6:8]))
	} else {
		switch m := string(hdr[:4]); m {
		case "BEEF":
			typ = binary.BigEndian.Uint16(hdr[4:6])
			n = int(binary.BigEndian.Uint16(hdr[6:8]))
		default:
			// In-band control frames ("APCP"/"ASCP" carry a u32 total length
			// at [4:8]; "MGMT"/"CSCO" a u16 at [6:8]). Skip by their length.
			if u32 := binary.BigEndian.Uint32(hdr[4:8]); u32 >= minFrameLen && u32 <= maxFrameLen && (m == "APCP" || m == "ASCP") {
				n = int(u32)
			} else {
				n = int(binary.BigEndian.Uint16(hdr[6:8]))
				if n < minFrameLen {
					n = minFrameLen // conservative skip on unknown magic
				}
			}
			if n < frameHeaderLen || n > maxFrameLen {
				return nil, fmt.Errorf("avct: in-band %q bad length %d", m, n)
			}
			body := make([]byte, n-frameHeaderLen)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, err
			}
			return nil, nil //nolint:nilnil // in-band frames carry no AVSP packet
		}
	}
	if n < minFrameLen || n > maxFrameLen {
		return nil, fmt.Errorf("avct: bad frame length %d", n)
	}
	body := make([]byte, n-frameHeaderLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return &Frame{Type: typ, Body: body}, nil
}

// WriteFrame writes f on the control channel.
func WriteFrame(w io.Writer, f *Frame) error {
	_, err := w.Write(f.Bytes())
	return err
}

func addr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
