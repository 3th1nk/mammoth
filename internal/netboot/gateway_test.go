package netboot

import (
	"net"
	"strings"
	"testing"
)

// Sample shaped like a real /proc/net/route on a docker host: multiple
// interfaces, per-interface subnet routes, one default route on ens192
// (via 192.168.1.1), an onlink default on a vpn tun (gateway all-zero),
// and a second default on another interface.
const routeTable = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
ens192	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
ens192	0000A8C0	00000000	0001	0	0	100	0000FFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
br-92d6	002011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
tun0	00000000	00000000	0003	0	0	50	00000000	0	0	0
wg0	00000000	FE01A8C0	0003	0	0	20	00000000	0	0	0
`

func TestGatewayFromRouteTable(t *testing.T) {
	gw, ok := gatewayFromRouteTable(strings.NewReader(routeTable), "ens192")
	if !ok || !gw.Equal(net.IPv4(192, 168, 1, 1)) {
		t.Fatalf("ens192 default = %v ok=%v, want 192.168.1.1/true", gw, ok)
	}

	// An interface without a default route reports ok=false — the caller
	// omits the router option rather than guessing.
	if gw, ok := gatewayFromRouteTable(strings.NewReader(routeTable), "docker0"); ok {
		t.Fatalf("docker0 has no default route, got %v", gw)
	}

	// A default whose gateway is all-zero is onlink, not a gateway.
	if gw, ok := gatewayFromRouteTable(strings.NewReader(routeTable), "tun0"); ok {
		t.Fatalf("tun0's onlink default must not surface as a gateway, got %v", gw)
	}

	// A second interface's default is found independently.
	if gw, ok := gatewayFromRouteTable(strings.NewReader(routeTable), "wg0"); !ok || !gw.Equal(net.IPv4(192, 168, 1, 254)) {
		t.Fatalf("wg0 default = %v ok=%v, want 192.168.1.254/true", gw, ok)
	}

	// Missing interface: ok=false, no panic.
	if _, ok := gatewayFromRouteTable(strings.NewReader(routeTable), "eth9"); ok {
		t.Fatal("unknown interface must report ok=false")
	}

	// Malformed lines are skipped, not fatal (a valid 8-field row still wins).
	if _, ok := gatewayFromRouteTable(strings.NewReader("garbage line\n\nens192\t00000000\tzzzzzzzz\nens192\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"), "ens192"); !ok {
		t.Fatal("malformed lines must be skipped, valid default still found")
	}
}

func TestLittleEndianHexIP(t *testing.T) {
	cases := map[string]string{
		"0101A8C0": "192.168.1.1",
		"FE01A8C0": "192.168.1.254",
		"00000000": "0.0.0.0",
	}
	for in, want := range cases {
		got := littleEndianHexIP(in)
		if got == nil || got.String() != want {
			t.Errorf("littleEndianHexIP(%q) = %v, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "0101A8C", "zzzzzzzz", "0101A8C0FF"} {
		if littleEndianHexIP(bad) != nil {
			t.Errorf("littleEndianHexIP(%q) must be nil", bad)
		}
	}
}
