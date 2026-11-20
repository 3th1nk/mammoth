package provision

import (
	"net"
	"testing"
)

// TestSameSegment pins the pool-reservation guard's subnet math: a reserved
// pool address and the machine's BMC address must share the pool's subnet
// before the static ip= argument is rendered — a foreign-segment static
// would strand an edge-served machine (its installer falls back to ip=dhcp
// and that segment's site DHCP answers).
func TestSameSegment(t *testing.T) {
	mask24 := net.IPMask([]byte{255, 255, 255, 0})
	cases := []struct {
		name string
		ip   string
		addr string
		mask net.IPMask
		want bool
	}{
		{"same /24 segment", "192.168.1.10", "192.168.1.99", mask24, true},
		{"different /24 segment", "192.168.1.10", "192.168.2.99", mask24, false},
		{"/16 boundary inside", "10.1.2.3", "10.1.99.99", net.IPMask([]byte{255, 255, 0, 0}), true},
		{"nil mask is unjudgeable", "10.0.0.5", "10.0.0.5", nil, false},
		{"nil address", "10.0.0.5", "", mask24, false},
		{"nil pool ip", "", "10.0.0.5", mask24, false},
		{"non-v4 mask length", "10.0.0.5", "10.0.0.5", net.IPMask([]byte{255, 255, 0, 0, 0, 0}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ip, addr net.IP
			if c.ip != "" {
				ip = net.ParseIP(c.ip)
			}
			if c.addr != "" {
				addr = net.ParseIP(c.addr)
			}
			if got := sameSegment(ip, addr, c.mask); got != c.want {
				t.Errorf("sameSegment(%v, %v, %v) = %v, want %v", c.ip, c.addr, c.mask, got, c.want)
			}
		})
	}
}
