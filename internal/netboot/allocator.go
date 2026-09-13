package netboot

import (
	"net"
	"sync"
	"time"
)

// DHCPPool is the optional address allocator behind MAMMOTH_PXE_DHCP_POOL.
// Machine rooms without a site DHCP are common (the reference deployment
// has none): UEFI PXE ROMs need an IP lease before they will fetch anything,
// so a pure proxy responder strands them. With a pool configured, the
// responder answers PXE clients (option 60) with a full DHCP OFFER/ACK —
// and only PXE clients: ordinary hosts on the L2 are left to the site DHCP,
// keeping the two modes separable on purpose (pool mode assumes it is the
// address authority for the machines it installs).
//
// Leases are in-memory and volatile by design: they only need to cover the
// boot + install window, and a rebooting machine simply re-requests.
type DHCPPool struct {
	start  net.IP
	end    net.IP
	mask   net.IP
	router net.IP

	mu     sync.Mutex
	next   uint32
	leases map[string]dhcpLease
}

type dhcpLease struct {
	ip      net.IP
	expires time.Time
}

const dhcpLeaseTTL = 15 * time.Minute

// NewDHCPPool validates and builds a pool from "start,end" (inclusive,
// same subnet). mask/router optional: mask defaults to /24, router to the
// responder's own next-server address.
func NewDHCPPool(start, end, mask, router net.IP) (*DHCPPool, error) {
	s, e := start.To4(), end.To4()
	if s == nil || e == nil {
		return nil, errPoolRange
	}
	if compareIP(s, e) > 0 {
		s, e = e, s
	}
	if mask == nil || mask.To4() == nil {
		mask = net.IP([]byte{255, 255, 255, 0})
	}
	if router != nil {
		router = router.To4()
	}
	return &DHCPPool{start: s, end: e, mask: mask, router: router,
		leases: map[string]dhcpLease{}}, nil
}

var errPoolRange = &net.AddrError{Err: "invalid dhcp pool range", Addr: ""}

// lease returns the address for a MAC, reserving the next free one on
// first sight and keeping the assignment sticky afterwards (a PXE client
// DISCOVERs, then REQUESTs, then the installer renews — same MAC, same IP).
func (p *DHCPPool) lease(mac string) net.IP {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if l, ok := p.leases[mac]; ok && now.Before(l.expires) {
		l.expires = now.Add(dhcpLeaseTTL)
		p.leases[mac] = l
		return l.ip
	}
	start, end := binaryIP(p.start), binaryIP(p.end)
	// Walk the range from p.next, wrapping once; skip live leases of other
	// MACs. Expired leases are reusable on the way.
	n := p.next
	if n < start || n > end {
		n = start
	}
	for {
		ip := ipFromBinary(n)
		taken := false
		for m, l := range p.leases {
			if l.ip.Equal(ip) && m != mac && now.Before(l.expires) {
				taken = true
				break
			}
		}
		n++
		if n > end {
			n = start
		}
		if !taken {
			p.next = n
			p.leases[mac] = dhcpLease{ip: ip, expires: now.Add(dhcpLeaseTTL)}
			return ip
		}
		if n == start { // full wrap without a free address
			return nil
		}
	}
}

// Mask and Router are the lease parameters handed to clients.
func (p *DHCPPool) Mask() net.IP {
	if p == nil {
		return nil
	}
	return p.mask
}

func (p *DHCPPool) Router() net.IP {
	if p == nil {
		return nil
	}
	return p.router
}

// binaryIP is the big-endian uint32 view of a 4-byte IPv4 address.
func binaryIP(ip net.IP) uint32 {
	v := ip.To4()
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func ipFromBinary(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func compareIP(a, b net.IP) int {
	x, y := binaryIP(a), binaryIP(b)
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}
