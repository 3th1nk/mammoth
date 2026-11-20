package netboot

import (
	"net"
	osexec "os/exec"
	"strings"
)

// ProbeAlive reports whether ip answers on the wire — the liveness gate for
// pool allocations (ReserveFree / SetProbe). Static devices that never speak
// DHCP but answer ping or resolve in ARP share the provisioning L2 (2288H
// machine room: a VMware VM held .191, and two ICMP-blackholed hosts sat on
// .194/.195 — ping alone misses those, the neighbour-table check after the
// ping catches them). A probe failure (no ping binary, container without
// NET_RAW) reads as not-alive: the gate degrades to "trust the pool range".
func ProbeAlive(ip net.IP) bool {
	_ = osexec.Command("ping", "-c1", "-W1", ip.String()).Run()
	out, err := osexec.Command("ip", "neigh", "show", ip.String()).Output()
	if err != nil {
		return false
	}
	line := string(out)
	return strings.Contains(line, "REACHABLE") || strings.Contains(line, "STALE") ||
		strings.Contains(line, "DELAY") || strings.Contains(line, "PROBE")
}
