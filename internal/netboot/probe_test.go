package netboot

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestClassifyMatrix(t *testing.T) {
	two := []Offer{{ServerID: "10.0.0.1", Yiaddr: "10.0.0.50"}, {ServerID: "10.0.0.2"}}
	one := []Offer{{ServerID: "10.0.0.1"}}
	cases := []struct {
		name  string
		mode  ProbeMode
		raw   []Offer
		kind  VerdictKind
		level string
	}{
		{"pool racing a site DHCP is the conflict shape", ProbeModePool, two, ProbeAddrConflict, "warn"},
		{"pool alone is healthy", ProbeModePool, nil, ProbeHealthy, "info"},
		{"proxy with a site DHCP is healthy", ProbeModeProxy, one, ProbeHealthy, "info"},
		{"proxy without a site DHCP strands machines", ProbeModeProxy, nil, ProbeNoAddressService, "warn"},
		{"external without a site DHCP strands machines", ProbeModeExternal, nil, ProbeNoAddressService, "warn"},
		{"external with a site DHCP is healthy", ProbeModeExternal, two, ProbeHealthy, "info"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Classify(c.mode, c.raw)
			if v.Kind != c.kind || v.Level != c.level {
				t.Errorf("Classify(%s) = %s/%s, want %s/%s", c.mode, v.Kind, v.Level, c.kind, c.level)
			}
		})
	}
	conflict := Classify(ProbeModePool, two)
	if !strings.Contains(conflict.Detail, "MAMMOTH_PXE_DHCP_POOL") {
		t.Errorf("pool conflict detail must name the knob: %q", conflict.Detail)
	}
	if ids := ServerIDs(two); ids != "10.0.0.1, 10.0.0.2" {
		t.Errorf("serverIDs not sorted: %q", ids)
	}
}

// TestBuildDiscoverPlain pins the payload contract: a plain DISCOVER with the
// broadcast flag and NO PXEClient class — the shape that keeps the engine's
// own responders and OnObserve silent while a site DHCP answers.
func TestBuildDiscoverPlain(t *testing.T) {
	b := BuildDiscover(0xdeadbeef, probeMAC)
	p, err := parse(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mt := p.messageType(); mt != msgDiscover {
		t.Errorf("message type = %d, want DISCOVER(%d)", mt, msgDiscover)
	}
	if p.flags&0x8000 == 0 {
		t.Error("broadcast flag must be set — the probe has no address to be unicasted to")
	}
	if p.isPXEClient() {
		t.Error("option 60 must be absent: the engine's proxy gate and OnObserve key on it")
	}
	if _, ok := p.options[optVendorClass]; ok {
		t.Error("no vendor class option at all")
	}
	if got := p.mac(); got != normalizeMAC("02:4d:41:4d:4d:4f") {
		t.Errorf("chaddr = %q", got)
	}
	if p.xid != 0xdeadbeef {
		t.Errorf("xid = %x", p.xid)
	}
}

// fakeOfferServer answers the first DISCOVER with `replies` identical OFFERs
// from serverID over a real loopback socket (unicast to the probe's source —
// the RFC 2131 shape for a client that already holds a source port).
// Multiple replies from one server model the PXE retry noise the probe's
// dedup must absorb.
func fakeOfferServer(t *testing.T, serverID string, replies int) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		p, perr := parse(buf[:n])
		if perr != nil || p.messageType() != msgDiscover {
			return
		}
		rep := p.bytes(msgOffer, "undionly.kpxe", option{optServerID, net.ParseIP(serverID).To4()})
		copy(rep[16:20], []byte{10, 0, 0, 50}) // yiaddr: a real OFFER carries the leased address
		for i := 0; i < replies; i++ {
			c.WriteToUDP(rep, src)
		}
	}()
	return c
}

// TestProbeDedupsRetryNoise drives the real socket exchange: two identical
// OFFERs from one server within the window collapse to one entry, and the
// parsed identity fields carry through.
func TestProbeDedupsRetryNoise(t *testing.T) {
	srv := fakeOfferServer(t, "10.0.0.1", 2)
	offers, err := Probe(context.Background(), ProbeOptions{
		Target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: srv.LocalAddr().(*net.UDPAddr).Port},
		Window: 2 * time.Second,
		XID:    0x1234,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("offers = %d (%+v), want 1 after dedup", len(offers), offers)
	}
	o := offers[0]
	if o.ServerID != "10.0.0.1" {
		t.Errorf("serverID = %q", o.ServerID)
	}
	if o.Bootfile != "undionly.kpxe" {
		t.Errorf("bootfile = %q", o.Bootfile)
	}
	if o.Yiaddr == "" {
		t.Error("yiaddr must survive the round trip")
	}
}

// TestProbeSilence pins the silence contract: an empty window is a
// classification input, not an error.
func TestProbeSilence(t *testing.T) {
	offers, err := Probe(context.Background(), ProbeOptions{
		Target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: freeUDPPort(t)},
		Window: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("silence must not be an error: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("offers = %+v, want none", offers)
	}
}
