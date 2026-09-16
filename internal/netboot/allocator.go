package netboot

import (
	"fmt"
	"net"
	"strconv"
	"strings"
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
// Addresses come either from a contiguous range or an explicit list
// (mutually exclusive; see ParseDHCPPool).
type DHCPPool struct {
	start  net.IP
	end    net.IP
	addrs  []net.IP // explicit list mode; start/end nil
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

// NewDHCPPool validates and builds a pool from an inclusive contiguous
// range (same subnet). mask/router optional: mask defaults to /24, router
// to the responder's own next-server address.
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

// NewDHCPPoolList builds a pool from explicit addresses (comma-separated
// form of MAMMOTH_PXE_DHCP_POOL): only these addresses are ever leased.
func NewDHCPPoolList(addrs []net.IP, mask, router net.IP) (*DHCPPool, error) {
	var v4 []net.IP
	for _, a := range addrs {
		if a.To4() == nil {
			return nil, fmt.Errorf("dhcp pool address %q is not IPv4", a)
		}
		v4 = append(v4, a.To4())
	}
	if len(v4) == 0 {
		return nil, fmt.Errorf("dhcp pool list is empty")
	}
	if mask == nil || mask.To4() == nil {
		mask = net.IP([]byte{255, 255, 255, 0})
	}
	if router != nil {
		router = router.To4()
	}
	return &DHCPPool{addrs: v4, mask: mask, router: router,
		leases: map[string]dhcpLease{}}, nil
}

// ParseDHCPPool parses the pool specification, one of:
//
//	10.0.0.10-10.0.0.50   dash range, both bounds full
//	10.0.0.10-50          dash range, last-octet shorthand
//	10.0.0.10,10.0.0.20   comma list of individual addresses
//
// CIDR is deliberately not accepted — pools rarely sit on network-aligned
// boundaries, and a lease range must exclude the network and broadcast
// addresses, which CIDR notation would silently include. Range and list do
// not mix within one spec.
func ParseDHCPPool(spec string, router net.IP) (*DHCPPool, error) {
	if !strings.Contains(spec, "-") {
		var addrs []net.IP
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			ip := net.ParseIP(part)
			if ip == nil || ip.To4() == nil {
				return nil, fmt.Errorf("dhcp pool address %q is not an IPv4 address", part)
			}
			addrs = append(addrs, ip)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("dhcp pool %q names no addresses", spec)
		}
		return NewDHCPPoolList(addrs, nil, router)
	}

	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return nil, fmt.Errorf("dhcp pool %q is not a range (want start-end, e.g. 10.0.0.10-10.0.0.50 or 10.0.0.10-50)", spec)
	}
	start := net.ParseIP(strings.TrimSpace(startStr))
	if start == nil || start.To4() == nil {
		return nil, fmt.Errorf("dhcp pool start %q is not an IPv4 address", strings.TrimSpace(startStr))
	}
	endStr = strings.TrimSpace(endStr)
	end := net.ParseIP(endStr)
	if end == nil {
		// Last-octet shorthand: digits only, prefix inherited from start.
		if isDigits(endStr) {
			v4 := start.To4()
			if n, err := strconv.Atoi(endStr); err == nil && n < 256 {
				end = net.IPv4(v4[0], v4[1], v4[2], byte(n)).To4()
			}
		}
	}
	if end == nil || end.To4() == nil {
		return nil, fmt.Errorf("dhcp pool end %q is not an IPv4 address", endStr)
	}
	return NewDHCPPool(start, end, nil, router)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

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
	if p.addrs != nil {
		// List mode: first address not live-leased to another MAC.
		for _, ip := range p.addrs {
			taken := false
			for m, l := range p.leases {
				if m != mac && l.ip.Equal(ip) && now.Before(l.expires) {
					taken = true
					break
				}
			}
			if !taken {
				p.leases[mac] = dhcpLease{ip: ip, expires: now.Add(dhcpLeaseTTL)}
				return ip
			}
		}
		return nil
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

// macFor returns the MAC holding a live lease for ip, or "" if none. It is
// the reverse of lease: the TFTP grub.cfg renderer resolves a client by its
// lease IP, because Debian grubnet requests the fixed path /grub/grub.cfg
// (proxyDHCP leaves net_default_server empty, so the per-MAC filename
// variants are never tried).
func (p *DHCPPool) macFor(ip net.IP) string {	if p == nil || ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for mac, l := range p.leases {
		if l.ip.Equal(v4) && now.Before(l.expires) {
			return mac
		}
	}
	return ""
}

// LeaseFor returns the live lease IP for mac, or nil — a pure read, never
// reserving (the allocating path is lease, the DHCP read path). Provision's
// verify_ready uses it as the address fallback for DHCP-carrier installs
// (ubuntu PXE: live system and target both DHCP, so the machine answers on
// its lease, not on any recorded static address). MAC spelling is normalized
// on both sides — the pool keys chaddr spellings, machine records carry
// Redfish spellings, and the two differ in case and separators.
func (p *DHCPPool) LeaseFor(mac string) net.IP {
	if p == nil || mac == "" {
		return nil
	}
	want := normalizeMAC(mac)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for m, l := range p.leases {
		if normalizeMAC(m) == want && now.Before(l.expires) {
			return l.ip
		}
	}
	return nil
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
