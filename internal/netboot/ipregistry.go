package netboot

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// attributionGrace is how long a released task's attributions keep
// resolving after Forget. The completion report races the installer's own
// final syslog lines — subiquity/d-i keep emitting umount/reboot chatter
// for a few seconds after the engine has already taken the release path —
// and the release must not swallow the closing evidence.
const attributionGrace = 2 * time.Minute

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
	mu    sync.Mutex
	grace time.Duration            // Forget's resolve window; tests inject a short one
	byIP  map[string]ipAttribution // key: bare address string
}

type ipAttribution struct {
	taskID  string
	expires time.Time
}

func NewIPRegistry() *IPRegistry {
	return &IPRegistry{grace: attributionGrace, byIP: map[string]ipAttribution{}}
}

// Register maps the task's addresses to its id until the TTL. Entries may
// be bare addresses or CIDR declarations — the spec's addresses are CIDR
// form (render.NetworkEntry carries "192.0.2.7/24"), and a strict ParseIP
// here used to drop every one of them, leaving declared-static installs
// permanently unattributed. Re-registration (a retried task re-prepares)
// overwrites; Forget (the release paths) ends them after a grace window.
// Nil-receiver safe — callers wire the registry optionally.
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
		ip := attributionIP(s)
		if ip == "" {
			continue
		}
		r.byIP[ip] = ipAttribution{taskID: taskID, expires: exp}
	}
}

// attributionIP normalizes a spec address into the bare form the registry
// keys on: CIDR declarations keep their host address, bare IPs pass
// through, anything else is unownable (""). ParsePrefix, not ParseCIDR —
// the latter masks the network address out, losing the host.
func attributionIP(s string) string {
	if net.ParseIP(s) != nil {
		return s
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr().String()
	}
	return ""
}

// Forget ends the task's attributions early (the release paths): the TTL
// is shortened to now+grace rather than dropped outright, so syslog lines
// still in flight when the release runs keep resolving through the
// installer's closing seconds. A retried task re-registers on top; expired
// entries sweep out lazily on the next Register.
func (r *IPRegistry) Forget(taskID string) {
	if r == nil || taskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	grace := r.grace
	if grace <= 0 {
		grace = attributionGrace
	}
	cutoff := time.Now().Add(grace)
	for ip, a := range r.byIP {
		if a.taskID == taskID && a.expires.After(cutoff) {
			r.byIP[ip] = ipAttribution{taskID: taskID, expires: cutoff}
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
