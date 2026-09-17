package netboot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

func TestDHCPPoolLease(t *testing.T) {
	pool, err := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 203), nil, net.IPv4(192, 168, 77, 1))
	if err != nil {
		t.Fatal(err)
	}

	// Sticky: the same MAC keeps its lease across DISCOVER → REQUEST → renew.
	a := pool.lease("52:54:00:00:00:01")
	b := pool.lease("52:54:00:00:00:01")
	if a == nil || !a.Equal(b) {
		t.Fatalf("lease not sticky: %v vs %v", a, b)
	}
	if !bytes.Equal(a.To4(), []byte{192, 168, 77, 200}) {
		t.Fatalf("first lease = %v, want the range start", a)
	}

	// Sequential MACs get distinct addresses from the range.
	seen := map[string]bool{a.String(): true}
	for _, mac := range []string{"52:54:00:00:00:02", "52:54:00:00:00:03", "52:54:00:00:00:04"} {
		ip := pool.lease(mac)
		if ip == nil || seen[ip.String()] {
			t.Fatalf("lease %v duplicate or nil for %s", ip, mac)
		}
		seen[ip.String()] = true
	}

	// Exhaustion: the 5th MAC finds nothing (range has 4 addresses).
	if ip := pool.lease("52:54:00:00:00:05"); ip != nil {
		t.Fatalf("pool not exhausted: %v", ip)
	}
}

func TestDHCPPoolSwappedBounds(t *testing.T) {
	pool, err := NewDHCPPool(net.IPv4(10, 0, 0, 5), net.IPv4(10, 0, 0, 3), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for want := byte(3); want <= 5; want++ {
		ip := pool.lease("52:54:00:00:00:0" + string('0'+want))
		if ip == nil || !ip.To4().Equal(net.IPv4(10, 0, 0, want).To4()) {
			t.Fatalf("lease = %v, want 10.0.0.%d", ip, want)
		}
	}
}

func TestNewDHCPPoolRejectsGarbage(t *testing.T) {
	if _, err := NewDHCPPool(net.ParseIP("nonsense"), net.IPv4(1, 2, 3, 4), nil, nil); err == nil {
		t.Fatal("non-IP bounds must be rejected")
	}
}

// LeaseFor is the read-only forward lookup verify_ready uses: it finds a
// live lease in any spelling, never reserves for unknown MACs, and returns
// nil after expiry.
func TestDHCPPoolLeaseFor(t *testing.T) {
	pool, err := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 203), nil, net.IPv4(192, 168, 77, 1))
	if err != nil {
		t.Fatal(err)
	}
	ip := pool.lease("52:54:00:00:00:01")
	if ip == nil {
		t.Fatal("no lease to find")
	}

	// Any common spelling finds the lease; the lookup never allocates.
	for _, mac := range []string{"52:54:00:00:00:01", "52-54-00-00-00-01", "525400000001", "5254:0000:0001"} {
		got := pool.LeaseFor(mac)
		if got == nil || !got.Equal(ip) {
			t.Fatalf("LeaseFor(%q) = %v, want %v", mac, got, ip)
		}
	}
	if got := pool.LeaseFor("52:54:00:00:00:09"); got != nil {
		t.Fatalf("LeaseFor reserved for an unknown MAC: %v", got)
	}
	if got := pool.LeaseFor(""); got != nil {
		t.Fatalf("LeaseFor(\"\") = %v", got)
	}
	var nilPool *DHCPPool
	if got := nilPool.LeaseFor("52:54:00:00:00:01"); got != nil {
		t.Fatalf("nil pool returned %v", got)
	}
}

func TestDHCPPoolMacFor(t *testing.T) {
	pool, _ := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 203), nil, net.IPv4(192, 168, 77, 1))
	ip := pool.lease("52:54:00:00:00:01")
	if ip == nil {
		t.Fatal("lease failed")
	}
	if got := pool.macFor(ip); got != "52:54:00:00:00:01" {
		t.Errorf("macFor(%v) = %q, want the leasing MAC", ip, got)
	}
	if got := pool.macFor(net.IPv4(192, 168, 77, 250)); got != "" {
		t.Errorf("macFor(unleased) = %q, want empty", got)
	}
	if got := pool.macFor(net.IPv6loopback); got != "" {
		t.Errorf("macFor(non-v4) = %q, want empty", got)
	}
}

// TestPoolModeReply pins the full-DHCP reply shape: yiaddr from the pool,
// lease time, mask and router options — the OFFER a bare-L2 boot ROM needs.
func TestPoolModeReply(t *testing.T) {
	pool, _ := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 200), nil, net.IPv4(192, 168, 77, 1))
	s := testServer(t, Options{
		NextServer: []byte{192, 168, 77, 1},
		BaseURL:    "http://192.168.77.1:8080",
		DHCP:       pool,
	})
	var mac [6]byte
	copy(mac[:], []byte{0x50, 0x1d, 0x93, 0xd8, 0xc6, 0x97})
	req := discover(0x1111, mac,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	)
	setBroadcastFlag(req)
	reply, to := s.handle(req, 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68})
	if reply == nil {
		t.Fatal("want a reply")
	}
	if to == nil || !to.IP.Equal(net.IPv4bcast) || to.Port != 68 {
		t.Fatalf("reply addr = %v", to)
	}
	p, err := parse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if got := net.IP(p.yiaddr[:]).String(); got != "192.168.77.200" {
		t.Errorf("yiaddr = %v, want the leased pool address", net.IP(p.yiaddr[:]))
	}
	if lt, ok := p.options[optLeaseTime]; !ok || binary.BigEndian.Uint32(lt) == 0 {
		t.Errorf("lease time missing: %v", lt)
	}
	if m, ok := p.options[optSubnetMask]; !ok || !bytes.Equal(m, []byte{255, 255, 255, 0}) {
		t.Errorf("subnet mask = %v", m)
	}
	if r, ok := p.options[optRouter]; !ok || !net.IP(r).Equal(net.IPv4(192, 168, 77, 1)) {
		t.Errorf("router = %v", r)
	}

	// REQUEST follows: ACK with the same address.
	req2 := discover(0x1112, mac,
		option{optMessageType, []byte{msgRequest}},
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optServerID, []byte{192, 168, 77, 1}},
	)
	setBroadcastFlag(req2)
	reply2, _ := s.handle(req2, 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68})
	p2, _ := parse(reply2)
	if p2.messageType() != msgAck {
		t.Fatalf("want ACK, got %d", p2.messageType())
	}
	if !bytes.Equal(p2.yiaddr[:], p.yiaddr[:]) {
		t.Fatalf("ACK offered a different address: %v vs %v", net.IP(p2.yiaddr[:]), net.IP(p.yiaddr[:]))
	}
	_ = context.Background
}

// TestPoolModeServesInstallerKernel pins the dracut case: the installer's
// own DHCP carries no option 60 and must get a lease (no boot parameters —
// a plain host lease), or the install stalls forever (2288H finding).
func TestPoolModeServesInstallerKernel(t *testing.T) {
	pool, _ := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 201), nil, net.IPv4(192, 168, 77, 1))
	s := testServer(t, Options{
		NextServer: []byte{192, 168, 77, 1},
		BaseURL:    "http://192.168.77.1:8080",
		DHCP:       pool,
	})
	var mac [6]byte
	copy(mac[:], []byte{0x50, 0x1d, 0x93, 0xd8, 0xc6, 0x97})

	// Plain DISCOVER (no option 60): proxy mode would stay silent, pool
	// mode answers with a bare lease.
	req := discover(0x2222, mac, option{optMessageType, []byte{msgDiscover}})
	setBroadcastFlag(req)
	reply, to := s.handle(req, 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68})
	if reply == nil || to == nil || !to.IP.Equal(net.IPv4bcast) {
		t.Fatalf("pool mode must lease plain hosts: reply=%v to=%v", reply != nil, to)
	}
	p, _ := parse(reply)
	if got := net.IP(p.yiaddr[:]).String(); got != "192.168.77.200" {
		t.Fatalf("yiaddr = %v", got)
	}
	if f := hexClean(p.file[:]); f != "" {
		t.Fatalf("plain host must not receive a bootfile, got %q", f)
	}
	if _, ok := p.options[optBootfile]; ok {
		t.Error("opt67 must be absent for plain hosts")
	}
	if string(p.yiaddr[:]) == "" {
		t.Error("unreachable")
	}
	// Proxy mode without a pool stays silent for the same request.
	s2 := testServer(t, Options{NextServer: []byte{192, 168, 77, 1}, BaseURL: "http://x"})
	if r, _ := s2.handle(req, 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68}); r != nil {
		t.Error("proxy mode must not answer non-PXE clients")
	}
}

func TestParseDHCPPool(t *testing.T) {
	r := net.IPv4(10, 0, 0, 1).To4()

	// Full dash range.
	p, err := ParseDHCPPool("10.0.0.10-10.0.0.12", r)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.lease("aa:00:00:00:00:01").To4(); !bytes.Equal(got, []byte{10, 0, 0, 10}) {
		t.Fatalf("range start = %v", got)
	}

	// Last-octet shorthand.
	p, err = ParseDHCPPool("10.0.0.10-12", r)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.lease("aa:00:00:00:00:01").To4(); !bytes.Equal(got, []byte{10, 0, 0, 10}) {
		t.Fatalf("shorthand start = %v", got)
	}

	// Comma list: only the named addresses, in order.
	p, err = ParseDHCPPool("10.0.0.30, 10.0.0.20", r)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.lease("aa:00:00:00:00:01").To4(); !bytes.Equal(got, []byte{10, 0, 0, 30}) {
		t.Fatalf("list first = %v", got)
	}
	if got := p.lease("aa:00:00:00:00:02").To4(); !bytes.Equal(got, []byte{10, 0, 0, 20}) {
		t.Fatalf("list second = %v", got)
	}
	if got := p.lease("aa:00:00:00:00:03"); got != nil {
		t.Fatalf("list exhausted but leased %v", got)
	}

	// Rejections.
	for _, bad := range []string{
		"198.51.100.180,199", // shorthand on the wrong side of a comma
		"10.0.0.10-",        // missing end
		"nonsense-nonsense", // not IPs
	} {
		if _, err := ParseDHCPPool(bad, r); err == nil {
			t.Errorf("ParseDHCPPool(%q) accepted", bad)
		}
	}
}

// ReserveFree skips liveness-flagged addresses (static devices that never
// speak DHCP) and remembers the verdicts as phantom leases; a sticky lease
// for the MAC wins without probing.
func TestDHCPPoolReserveFree(t *testing.T) {
	pool, err := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 203), nil, net.IPv4(192, 168, 77, 1))
	if err != nil {
		t.Fatal(err)
	}
	alive := map[string]bool{"192.168.77.200": true, "192.168.77.201": true}
	probe := func(ip net.IP) bool { return alive[ip.String()] }

	ip, ok := pool.ReserveFree("52:54:00:00:00:aa", probe)
	if !ok || !ip.Equal(net.IPv4(192, 168, 77, 202)) {
		t.Fatalf("ReserveFree = %v,%v; want the first unprobed address", ip, ok)
	}
	// The verdicts became phantom leases: re-reserving another MAC skips
	// them without re-probing.
	ip2, ok := pool.ReserveFree("52:54:00:00:00:bb", nil)
	if !ok || !ip2.Equal(net.IPv4(192, 168, 77, 203)) {
		t.Fatalf("second ReserveFree = %v,%v; want .203", ip2, ok)
	}
	// Sticky: the first MAC keeps its address on the next arm.
	ipAgain, ok := pool.ReserveFree("52:54:00:00:00:aa", nil)
	if !ok || !ipAgain.Equal(ip) {
		t.Fatalf("sticky ReserveFree = %v,%v", ipAgain, ok)
	}
	// Exhaustion: all four addresses claimed.
	if _, ok := pool.ReserveFree("52:54:00:00:00:cc", nil); ok {
		t.Fatal("exhausted pool handed out an address")
	}
}

// The pool leases only MACs with an armed netboot entry: foreign DHCP
// clients on the wire (site VMs, stray servers) must not drain it
// (2288H machine room: a 2-address pool died mid-install to foreign
// REQUESTs). Without a Resolver the allowlist is skipped — legacy shape.
func TestPoolModeIgnoresUnknownMACs(t *testing.T) {
	pool, _ := NewDHCPPool(net.IPv4(192, 168, 77, 200), net.IPv4(192, 168, 77, 200), nil, net.IPv4(192, 168, 77, 1))
	s := testServer(t, Options{
		NextServer: []byte{192, 168, 77, 1},
		BaseURL:    "http://192.168.77.1:8080",
		DHCP:       pool,
		Resolver: ResolverFunc(func(_ context.Context, mac string) (*Entry, error) {
			if mac == "02:00:00:00:00:00" {
				return &Entry{MAC: mac}, nil
			}
			return nil, errors.New("unknown")
		}),
	})
	var mac [6]byte
	copy(mac[:], []byte{0x50, 0x1d, 0x93, 0xd8, 0xc6, 0x97})
	pxeOpts := []option{
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	}

	// Armed MAC: leased.
	setBroadcastFlag(discover(0x21, mac, pxeOpts...))
	reply, _ := s.handle(discover(0x21, mac, pxeOpts...), 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68})
	if reply == nil {
		t.Fatal("armed MAC got no lease")
	}

	// Foreign MAC (no entry): silently ignored — the pool keeps its
	// addresses for the machines it installs.
	var foreign [6]byte
	copy(foreign[:], []byte{0x00, 0x0c, 0x29, 0x9e, 0xd1, 0xc1})
	reply, _ = s.handle(discover(0x22, foreign, pxeOpts...), 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68})
	if reply != nil {
		t.Fatal("foreign MAC was leased")
	}
}
