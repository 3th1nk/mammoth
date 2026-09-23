package netboot

import (
	"net"
	"sync"
	"time"
)

// IPRegistry attributes syslog senders by IP address. The lease-reverse
// chain (DHCP.macFor → Resolver) only covers machines that hold one of our
// pool leases — vMedia machines have no lease at all, and site-DHCP proxy
// installs lease from the site. For those, provision registers the spec's
// declared static addresses here at boot time (TTL-bound; the terminal
// paths forget them) and serveSyslog falls back to this registry.
//
// An IP nobody declared has no honest owner: dynamic-address installs stay
// unattributed (source-IP-only logging), by design.
type IPRegistry struct {
	mu   sync.Mutex
	byIP map[string]ipAttribution // key: net.IP.String()
}

type ipAttribution struct {
	taskID  string
	expires time.Time
}

func NewIPRegistry() *IPRegistry {
	return &IPRegistry{byIP: map[string]ipAttribution{}}
}

// Register maps the task's declared addresses to its id until the TTL.
// Re-registration (a retried task re-prepares) overwrites; Forget (the
// release paths) drops them early. Nil-receiver safe — callers wire the
// registry optionally.
func (r *IPRegistry) Register(taskID string, ips []string, ttl time.Duration) {
	if r == nil || taskID == "" || len(ips) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for ip, a := range r.byIP { // lazy sweep keeps the map bounded
		if a.expires.Before(now) {
			delete(r.byIP, ip)
		}
	}
	exp := now.Add(ttl)
	for _, s := range ips {
		if net.ParseIP(s) == nil {
			continue
		}
		r.byIP[s] = ipAttribution{taskID: taskID, expires: exp}
	}
}

// Forget drops every attribution owned by the task (terminal paths).
func (r *IPRegistry) Forget(taskID string) {
	if r == nil || taskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ip, a := range r.byIP {
		if a.taskID == taskID {
			delete(r.byIP, ip)
		}
	}
}

// TaskForIP resolves a sender IP to its task id ("" when unknown or
// expired).
func (r *IPRegistry) TaskForIP(ip net.IP) string {
	if r == nil || ip == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byIP[ip.String()]
	if !ok || time.Now().After(a.expires) {
		return ""
	}
	return a.taskID
}
