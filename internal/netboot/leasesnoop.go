package netboot

import (
	"net"
	"sync"
	"time"
)

// LeaseSnoop records the address↔MAC bindings observed on PXE client
// traffic when mammoth is NOT the address authority (proxyDHCP: the site
// DHCP leases, we only watch the wire). The fixed-path /grub/grub.cfg
// render resolves clients through DHCP.macFor — the lease-reverse chain —
// which under proxyDHCP has no leases of its own to reverse: every UEFI
// Linux machine stranded at the grub> prompt with "no such file
// grub/grub.cfg" (2026-10-29 lab, rocky9 PXE + KVM UEFI guest).
// The client itself names the binding in the very REQUEST round that
// precedes the config fetch — chaddr next to option 50 / ciaddr / its
// source address — so watching the requests closes the gap without
// touching the site DHCP.
type LeaseSnoop struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	byIP map[string]snoopBinding
}

type snoopBinding struct {
	mac      string
	lastSeen time.Time
}

const (
	snoopTTL = 30 * time.Minute // one boot chain resolves in seconds; renewals re-learn
	snoopMax = 1024             // far past any provisioning L2's client count
)

// NewLeaseSnoop builds an empty table with the default TTL and capacity.
func NewLeaseSnoop() *LeaseSnoop {
	return newLeaseSnoop(snoopTTL, snoopMax)
}

func newLeaseSnoop(ttl time.Duration, max int) *LeaseSnoop {
	return &LeaseSnoop{ttl: ttl, max: max, byIP: map[string]snoopBinding{}}
}

// Observe records one binding; the newest observation wins and refreshes
// the TTL. Requesting the same address from a second NIC (a swapped
// machine behind a static DHCP reservation) re-points the binding.
func (s *LeaseSnoop) Observe(mac string, ip net.IP) {
	v4 := ip.To4()
	if mac == "" || v4 == nil || v4.IsUnspecified() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, done := s.byIP[v4.String()]; !done && len(s.byIP) >= s.max {
		s.evictLocked()
	}
	s.byIP[v4.String()] = snoopBinding{mac: mac, lastSeen: time.Now()}
}

// macFor returns the MAC last observed on ip, or "" when the binding is
// absent or older than the TTL.
func (s *LeaseSnoop) macFor(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byIP[v4.String()]
	if !ok || time.Since(b.lastSeen) > s.ttl {
		return ""
	}
	return b.mac
}

// evictLocked makes room for one more binding: drop the expired first,
// then the stalest (a linear scan — the table is bounded and this runs
// only on overflow of a fresh binding).
func (s *LeaseSnoop) evictLocked() {
	oldestKey := ""
	var oldest time.Time
	now := time.Now()
	for k, b := range s.byIP {
		if now.Sub(b.lastSeen) > s.ttl {
			delete(s.byIP, k)
			continue
		}
		if oldestKey == "" || b.lastSeen.Before(oldest) {
			oldestKey, oldest = k, b.lastSeen
		}
	}
	if len(s.byIP) >= s.max && oldestKey != "" {
		delete(s.byIP, oldestKey)
	}
}

// boundIP extracts the address a DHCP request says the client holds or
// wants: option 50 (selecting / init-reboot), then ciaddr (renew /
// rebind), then the datagram source (the port-4011 bootstrap REQUEST of an
// already-bound client). Nil for a broadcasting boot ROM that has no
// address yet — nothing to learn there.
func boundIP(p *packet, src *net.UDPAddr) net.IP {
	if v, ok := p.options[optRequestedIP]; ok && len(v) == 4 {
		if ip := net.IP(v).To4(); ip != nil && !ip.IsUnspecified() {
			return ip
		}
	}
	if ci := net.IP(p.ciaddr[:]).To4(); ci != nil && !ci.IsUnspecified() {
		return ci
	}
	if src != nil && src.IP != nil {
		if v4 := src.IP.To4(); v4 != nil && !v4.IsUnspecified() {
			return v4
		}
	}
	return nil
}
