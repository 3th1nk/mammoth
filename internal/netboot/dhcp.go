// Package netboot provides the network-boot services behind the "pxe" boot
// strategy: a proxyDHCP responder (Pixiecore model — mammoth never assigns
// addresses, the site DHCP does), a minimal TFTP server for the network boot
// programs (iPXE), and iPXE boot-script rendering. Install kernels/initrds
// ride HTTP on the machine face (/netboot/...), never TFTP — only the small
// NBP binary traverses TFTP.
package netboot

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DHCP wire constants (RFC 951/2131) — only what the proxy touches.
const (
	opRequest = 1
	opReply   = 2

	htypeEthernet = 1
	hlenEthernet  = 6

	optPad         = 0
	optEnd         = 255
	optMessageType = 53 // DHCP message type
	optServerID    = 54
	optClientID    = 61
	optVendorClass = 60
	optTFTPServer  = 66
	optBootfile    = 67
	optUserClass   = 77
	optArch        = 93  // client system architecture (RFC 4578)
	optFeatures    = 175 // Etherboot/iPXE feature indicators

	msgDiscover = 1
	msgOffer    = 2
	msgRequest  = 3
	msgAck      = 5
)

const magicCookie = "\x63\x82\x53\x63"

const (
	minPacketLen = 240 // BOOTP header (236) + magic cookie (4)
	fileFieldLen = 128 // BOOTP file field — some ROMs read it, not option 67
)

// ErrNotDHCP reports a datagram that is not a well-formed DHCP request.
var ErrNotDHCP = errors.New("netboot: not a dhcp request")

// Arch is a client firmware architecture (DHCP option 93, RFC 4578; the
// vendor class "PXEClient:Arch:xxxxx" carries the same value in hex).
type Arch string

const (
	ArchBIOS  Arch = "bios"
	ArchIA32  Arch = "ia32"
	ArchX64   Arch = "uefi-x64"
	ArchARM64 Arch = "uefi-arm64"
)

// archNumbers maps RFC 4578 processor architecture codes to Arch. Unknown
// codes stay unknown — the responder then stays silent and the firmware
// falls through to its next boot device.
var archNumbers = map[uint16]Arch{
	0: ArchBIOS,
	2: ArchIA32,
	6: ArchIA32,
	7: ArchX64,
	9: ArchARM64,
}

// archFromBytes decodes option 93 (a list of big-endian uint16 codes) by
// taking the first recognized entry.
func archFromBytes(v []byte) (Arch, bool) {
	for i := 0; i+2 <= len(v); i += 2 {
		if a, ok := archNumbers[binary.BigEndian.Uint16(v[i:i+2])]; ok {
			return a, true
		}
	}
	return "", false
}

// archFromVendorClass parses "PXEClient:Arch:00007:UNDI:003016" — the arch
// code is the third colon-separated field, in five hex digits.
func archFromVendorClass(v string) (Arch, bool) {
	parts := strings.Split(v, ":")
	if len(parts) < 3 || parts[0] != "PXEClient" || parts[1] != "Arch" {
		return "", false
	}
	var code uint16
	if _, err := fmt.Sscanf(parts[2], "%5x", &code); err != nil {
		return "", false
	}
	a, ok := archNumbers[code]
	return a, ok
}

// packet is a decoded DHCP request or a template for a reply.
type packet struct {
	op     byte
	htype  byte
	hlen   byte
	hops   byte
	xid    uint32
	secs   uint16
	flags  uint16
	ciaddr [4]byte
	yiaddr [4]byte
	siaddr [4]byte
	giaddr [4]byte
	chaddr [16]byte
	sname  [64]byte
	file   [fileFieldLen]byte

	options map[byte][]byte
}

// parse decodes a DHCP datagram, request or reply (the responder parses its
// own replies in tests; handle() gates requests itself). Datagrams without
// the magic cookie return ErrNotDHCP.
func parse(b []byte) (*packet, error) {
	if len(b) < minPacketLen || string(b[236:240]) != magicCookie {
		return nil, ErrNotDHCP
	}
	if b[0] != opRequest && b[0] != opReply {
		return nil, ErrNotDHCP
	}
	p := &packet{options: map[byte][]byte{}}
	p.op = b[0]
	p.htype = b[1]
	p.hlen = b[2]
	p.hops = b[3]
	p.xid = binary.BigEndian.Uint32(b[4:8])
	p.secs = binary.BigEndian.Uint16(b[8:10])
	p.flags = binary.BigEndian.Uint16(b[10:12])
	copy(p.ciaddr[:], b[12:16])
	copy(p.yiaddr[:], b[16:20])
	copy(p.siaddr[:], b[20:24])
	copy(p.giaddr[:], b[24:28])
	copy(p.chaddr[:], b[28:44])
	copy(p.sname[:], b[44:108])
	copy(p.file[:], b[108:236])
	for i := 240; i < len(b); {
		code := b[i]
		switch code {
		case optPad:
			i++
			continue
		case optEnd:
			return p, nil
		}
		if i+2 > len(b) {
			return p, nil // tolerate a truncated tail
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			return p, nil
		}
		p.options[code] = b[i+2 : i+2+l]
		i += 2 + l
	}
	return p, nil
}

// option is one TLV to append to a reply.
type option struct {
	code byte
	data []byte
}

// bytes serializes a BOOTREPLY echoing the request's identity fields
// (xid/chaddr/flags/giaddr — RFC 2131 §4.3.1) and carrying opts. The BOOTP
// file field is set alongside option 67: some PXE ROMs read only the field.
func (p *packet) bytes(msgType byte, file string, opts ...option) []byte {
	out := make([]byte, minPacketLen)
	out[0] = opReply
	out[1] = p.htype
	out[2] = p.hlen
	out[3] = 0 // hops
	binary.BigEndian.PutUint32(out[4:8], p.xid)
	binary.BigEndian.PutUint16(out[10:12], p.flags)
	copy(out[12:16], p.ciaddr[:])
	copy(out[24:28], p.giaddr[:])
	copy(out[28:44], p.chaddr[:])
	copy(out[236:240], magicCookie)
	copy(out[108:108+fileFieldLen], file) // BOOTP file field, NUL-padded

	put := func(code byte, data []byte) {
		out = append(out, code, byte(len(data)))
		out = append(out, data...)
	}
	put(optMessageType, []byte{msgType})
	if file != "" {
		put(optBootfile, []byte(file)) // option 67 alongside the BOOTP field
	}
	for _, o := range opts {
		put(o.code, o.data)
	}
	out = append(out, optEnd)
	return out
}

// mac returns the client hardware address normalized to lowercase
// colon-separated hex — the canonical key everywhere else (store rows,
// script URLs, cache). Falls back to a MAC-typed client identifier when the
// BOOTP field is empty (seen from some UEFI NICs).
func (p *packet) mac() string {
	if p.htype == htypeEthernet && p.hlen == hlenEthernet {
		return normalizeMAC(hex.EncodeToString(p.chaddr[:hlenEthernet]))
	}
	if cid, ok := p.options[optClientID]; ok && len(cid) == 7 && cid[0] == htypeEthernet {
		return normalizeMAC(hex.EncodeToString(cid[1:]))
	}
	return ""
}

// messageType returns option 53 of the request, or 0.
func (p *packet) messageType() byte {
	if v, ok := p.options[optMessageType]; ok && len(v) == 1 {
		return v[0]
	}
	return 0
}

// isPXEClient reports whether the request carries a PXEClient vendor class
// (option 60) — the gate for every reply the proxy sends.
func (p *packet) isPXEClient() bool {
	v, ok := p.options[optVendorClass]
	return ok && strings.HasPrefix(string(v), "PXEClient")
}

// isIPXE reports whether the requester already runs iPXE: it announces
// itself with user-class "iPXE" (option 77) and/or feature indicators
// (option 175). iPXE clients get the HTTP script URL instead of an NBP.
func (p *packet) isIPXE() bool {
	if _, ok := p.options[optFeatures]; ok {
		return true
	}
	if uc, ok := p.options[optUserClass]; ok {
		// RFC 3004 encodes each class as a length-prefixed string.
		for i := 0; i < len(uc); {
			l := int(uc[i])
			if i+1+l > len(uc) {
				break
			}
			if string(uc[i+1:i+1+l]) == "iPXE" {
				return true
			}
			i += 1 + l
		}
	}
	return false
}

// arch resolves the client architecture: option 93 first, then the vendor
// class fallback.
func (p *packet) arch() (Arch, bool) {
	if v, ok := p.options[optArch]; ok {
		if a, ok := archFromBytes(v); ok {
			return a, true
		}
	}
	if v, ok := p.options[optVendorClass]; ok {
		return archFromVendorClass(string(v))
	}
	return "", false
}

// normalizeMAC rewrites any common MAC spelling (AA-BB-.., AABBCCDDEEFF,
// aa:bb:..) into lowercase colon-separated hex. Input is not validated —
// callers pass hardware addresses from packets or the API.
func normalizeMAC(s string) string {
	s = strings.ToLower(strings.NewReplacer("-", "", ":", "", ".", "").Replace(s))
	if len(s) != 12 {
		return strings.ToLower(s) // not a MAC; let the caller's lookup miss
	}
	var b strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String()
}
