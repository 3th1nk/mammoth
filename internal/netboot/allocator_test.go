package netboot

import (
	"bytes"
	"context"
	"encoding/binary"
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
