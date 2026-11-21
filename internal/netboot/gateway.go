package netboot

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"strings"
)

// Pool-lease router derivation: DHCP server and gateway are independent
// roles, and the edge host — unlike a dedicated router — cannot know the
// segment's gateway by declaration alone. But it is ON the segment (the
// proxyDHCP/TFTP services are L2 by construction), so its own route table
// holds the one gateway that is verified to leave the segment: the default
// route via on the interface that owns next-server. Advertising that to
// pool clients means cross-segment boot traffic (iPXE's HTTP fetch of the
// engine script) follows a path this very host has proven, instead of a
// guessed convention (.1 is a habit, not a rule — .254, SVIs on arbitrary
// addresses and multi-homed designs all exist) or the edge host itself
// (which only routes if ip_forward is on AND the firewall lets it through;
// a docker host's FORWARD policy DROP does not — the 2026-11-21 .74 round
// timed out exactly there).
//
// Derivation failure (no default route on the service interface) is an
// isolated segment: the engine must be local for edge to reach it at all,
// so clients need no router option — omit it loudly rather than guess.

// DefaultGatewayFor returns the IPv4 default gateway of the interface that
// owns nextServer, read from the host route table. ok=false when the
// interface holds no default route or the table cannot be read (non-Linux
// dev hosts).
func DefaultGatewayFor(nextServer net.IP) (net.IP, bool) {
	iface, err := interfaceHolding(nextServer)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, false // non-Linux (dev builds) — caller warns and omits
	}
	return gatewayFromRouteTable(bytes.NewReader(data), iface)
}

// interfaceHolding resolves the interface name that owns addr.
func interfaceHolding(addr net.IP) (string, error) {
	want := addr.To4()
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.Contains(want) {
				return ifc.Name, nil
			}
		}
	}
	return "", errNoInterface{addr: want}
}

type errNoInterface struct{ addr net.IP }

func (e errNoInterface) Error() string {
	return "no local interface holds " + e.addr.String()
}

// gatewayFromRouteTable parses a /proc/net/route-shaped table and returns
// the default-route gateway for iface. The table's hex fields are
// little-endian (host uint32 on little-endian kernels), the format
// /proc/net/route has emitted since forever; the Destination for the
// default route is 00000000 and the gateway may be all-zero (onlink) —
// that is not a gateway, skip it.
func gatewayFromRouteTable(r io.Reader, iface string) (net.IP, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Iface") { // header
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != iface {
			continue
		}
		if fields[1] != "00000000" { // Destination: not the default route
			continue
		}
		gw := littleEndianHexIP(fields[2])
		if gw == nil || gw.IsUnspecified() {
			continue
		}
		return gw, true
	}
	return nil, false
}

// littleEndianHexIP parses an 8-hex-digit field as a little-endian uint32
// address ("0101A8C0" = 192.168.1.1).
func littleEndianHexIP(s string) net.IP {
	if len(s) != 8 {
		return nil
	}
	var raw [4]byte
	for i := 0; i < 4; i++ {
		hi, ok1 := hexVal(s[i*2])
		lo, ok2 := hexVal(s[i*2+1])
		if !ok1 || !ok2 {
			return nil
		}
		raw[3-i] = byte(hi<<4 | lo) // little-endian: bytes come out reversed
	}
	return net.IP(raw[:]).To4()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
