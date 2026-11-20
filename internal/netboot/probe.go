// Startup DHCP probe: one plain DISCOVER sent at service start, the OFFERs
// collected for a short window, and the segment's address-service shape
// classified against the engine's own PXE mode. The two failure shapes it
// catches — a pool-mode engine racing a site DHCP, and a proxy/external
// segment where nothing answers — are exactly the modes the runtime
// per-MAC observer can only attribute after real machines have been hurt.
//
// The DISCOVER deliberately carries NO PXEClient vendor class (option 60):
// the engine's own responders stay silent on it (proxydhcp.go gates every
// reply on isPXEClient; pool mode gates leases on armed entries), OnObserve
// never fires (no pending_machines row for the probe), while a site DHCP —
// a full DHCP with no such gate — answers normally. One payload, clean
// classification, zero self-interference and zero ledger pollution.
package netboot

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ProbeMode is the engine's address-service shape on the segment being
// probed (the same derivation serve.go makes for its DHCP responders).
type ProbeMode string

const (
	ProbeModePool     ProbeMode = "pool"     // engine owns addressing (PXEDHCPPool set)
	ProbeModeProxy    ProbeMode = "proxy"    // proxyDHCP only; the site DHCP owns addressing
	ProbeModeExternal ProbeMode = "external" // the site owns DHCP+TFTP entirely
)

// probeMAC is the probe's synthetic client hardware address: locally
// administered (02:) + the ASCII prefix "MAMMO". A site DHCP operator sees
// a self-describing source instead of a machine MAC, and the probe never
// occupies a lease that belongs to a real NIC.
var probeMAC = [6]byte{0x02, 0x4d, 0x41, 0x4d, 0x4d, 0x4f}

// Offer is one parsed DHCPOFFER (or a direct ACK — some servers lease on
// DISCOVER without a REQUEST; either proves the address service is alive).
type Offer struct {
	ServerID string // option 54 (dotted), BOOTP siaddr fallback
	Yiaddr   string // the offered address
	Bootfile string // option 67 or the BOOTP file field
}

// ProbeOptions carries what varies between production and tests: the target
// (broadcast in production, unicast loopback in tests), the source port
// (68 in production — DHCP servers reply there; ephemeral in tests, where
// binding a privileged port needs root), and the collection window.
type ProbeOptions struct {
	Target     *net.UDPAddr
	SourcePort int           // 0 lets the OS pick (tests); production passes 68
	Window     time.Duration // OFFER collection window
	XID        uint32        // 0 → random
}

// Probe sends one DISCOVER to o.Target and collects replies for o.Window.
// An empty result is NOT an error — silence is a classification input. The
// error covers only local failures (bind, send) that mean "the probe never
// went on the wire".
func Probe(ctx context.Context, o ProbeOptions) ([]Offer, error) {
	lc := net.ListenConfig{}
	lc.Control = func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if serr == nil {
				// Broadcast targets need SO_BROADCAST on the sender too.
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
	conn, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf(":%d", o.SourcePort))
	if err != nil {
		return nil, fmt.Errorf("dhcp probe: bind :%d: %w", o.SourcePort, err)
	}
	defer conn.Close()
	conn4 := conn.(*net.UDPConn)

	xid := o.XID
	if xid == 0 {
		xid = rand.Uint32()
	}
	discover := BuildDiscover(xid, probeMAC)
	if _, err := conn4.WriteToUDP(discover, o.Target); err != nil {
		return nil, fmt.Errorf("dhcp probe: send to %s: %w", o.Target, err)
	}

	deadline := time.Now().Add(o.Window)
	buf := make([]byte, 1500)
	seen := map[string]bool{}
	var offers []Offer
	for {
		conn4.SetReadDeadline(deadline)
		n, _, err := conn4.ReadFromUDP(buf)
		if err != nil {
			break // window closed — the normal exit, silence included
		}
		if ctx.Err() != nil {
			break
		}
		p, perr := parse(buf[:n])
		if perr != nil || p.op != opReply {
			continue
		}
		if mt := p.messageType(); mt != msgOffer && mt != msgAck {
			continue
		}
		offer := offerOf(p)
		if offer.ServerID == "" && offer.Yiaddr == "" {
			continue
		}
		key := offer.ServerID
		if key == "" {
			key = offer.Yiaddr
		}
		if seen[key] {
			continue // one reply per server — duplicate OFFERs are retry noise
		}
		seen[key] = true
		offers = append(offers, offer)
	}
	return offers, nil
}

// offerOf extracts the identity fields of a reply packet.
func offerOf(p *packet) Offer {
	o := Offer{Yiaddr: dotted(p.yiaddr[:])}
	if sid, ok := p.options[optServerID]; ok && len(sid) == 4 {
		o.ServerID = dotted(sid)
	}
	if o.ServerID == "" {
		o.ServerID = dotted(p.siaddr[:]) // BOOTP-era servers skip option 54
	}
	if f, ok := p.options[optBootfile]; ok {
		o.Bootfile = nullTerm(f)
	} else {
		o.Bootfile = nullTerm(p.file[:])
	}
	return o
}

// BuildDiscover constructs a minimal plain DHCPDISCOVER — no option 60, so
// the engine's own responders and the observer stay out of the way (package
// comment). The broadcast flag asks for a broadcast reply: the probe has no
// address to be unicasted to.
func BuildDiscover(xid uint32, mac [6]byte) []byte {
	b := make([]byte, minPacketLen)
	b[0], b[1], b[2], b[3] = opRequest, htypeEthernet, hlenEthernet, 0
	binary.BigEndian.PutUint32(b[4:8], xid)
	binary.BigEndian.PutUint16(b[10:12], 0x8000) // RFC 2131 §2: broadcast flag
	copy(b[28:34], mac[:])
	copy(b[236:240], magicCookie)
	put := func(code byte, data []byte) {
		b = append(b, code, byte(len(data)))
		b = append(b, data...)
	}
	put(optMessageType, []byte{msgDiscover})
	return append(b, optEnd)
}

// VerdictKind is the probe's classification of the segment.
type VerdictKind string

const (
	ProbeHealthy          VerdictKind = "healthy"                  // the shape matches the mode
	ProbeAddrConflict     VerdictKind = "address-service-conflict" // pool mode racing a site DHCP
	ProbeNoAddressService VerdictKind = "no-address-service"       // nothing answers where one must
)

// Verdict is the probe conclusion: what the segment looks like and what the
// operator should do about it. Level follows the startup-check precedent
// (builder.CheckToolchain): "info" logs, "warn" never fatal.
type Verdict struct {
	Kind   VerdictKind
	Level  string // "info" | "warn"
	Detail string // operator-facing sentence, server IDs included when known
}

// Classify compares the offers the probe collected against the engine's own
// mode. The matrix is the whole story: pool mode must be the ONLY address
// service (a site DHCP alongside it is a race), proxy and external modes
// REQUIRE one to answer.
func Classify(mode ProbeMode, offers []Offer) Verdict {
	ids := ServerIDs(offers)
	switch mode {
	case ProbeModePool:
		if len(offers) > 0 {
			return Verdict{ProbeAddrConflict, "warn", fmt.Sprintf(
				"pool-mode DHCP shares this segment with a site DHCP (%s) — the two address services will race; unset MAMMOTH_PXE_DHCP_POOL to run proxy mode, or remove the site DHCP from this VLAN", ids)}
		}
		return Verdict{ProbeHealthy, "info",
			"pool mode: no site DHCP answered — the engine is the segment's only address service"}
	case ProbeModeProxy:
		if len(offers) == 0 {
			return Verdict{ProbeNoAddressService, "warn",
				"no site DHCP answered — PXE clients on this segment get no address; set MAMMOTH_PXE_DHCP_POOL (pool mode) for this VLAN or repair the site DHCP"}
		}
		return Verdict{ProbeHealthy, "info",
			fmt.Sprintf("proxy mode: site DHCP answered (%s) — addresses and PXE are split as designed", ids)}
	default: // external
		if len(offers) == 0 {
			return Verdict{ProbeNoAddressService, "warn",
				"external mode assumes the site DHCP owns addressing — no DHCP answered, machines cannot boot"}
		}
		return Verdict{ProbeHealthy, "info",
			fmt.Sprintf("external mode: site DHCP answered (%s)", ids)}
	}
}

// ServerIDs renders the deduplicated OFFER sources as a sorted dotted-IP
// list — the payload/log form of the probe conclusion.
func ServerIDs(offers []Offer) string {
	if len(offers) == 0 {
		return "(none)"
	}
	ids := make([]string, 0, len(offers))
	for _, o := range offers {
		ids = append(ids, o.ServerID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

func dotted(b []byte) string {
	if len(b) != 4 || bytes.Equal(b, []byte{0, 0, 0, 0}) {
		return ""
	}
	return net.IP(b).String()
}

func nullTerm(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimRight(string(b), " \t")
}
