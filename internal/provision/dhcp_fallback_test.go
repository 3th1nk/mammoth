package provision

import (
	"encoding/json"
	"net"
	"testing"
)

// dhcpLeaseAddr is verify_ready's address fallback for DHCP-carrier installs
// (ubuntu PXE): the machine answers on its lease, not the recorded static.
func TestDHCPLeaseAddr(t *testing.T) {
	hw := json.RawMessage(`{"nics":[{"mac":"02:00:00:00:00:00"},{"mac":"02:00:00:00:00:01"}]}`)
	lease := map[string]string{
		"02:00:00:00:00:01": "192.168.77.182", // the NIC that actually DHCP'd
	}
	e := &Executor{DHCPLeaseFor: func(mac string) net.IP {
		if v, ok := lease[mac]; ok {
			return net.ParseIP(v)
		}
		return nil
	}}

	addr, ok := e.dhcpLeaseAddr(hw)
	if !ok || addr != "192.168.77.182" {
		t.Fatalf("dhcpLeaseAddr = %q,%v; want the leased NIC's address", addr, ok)
	}

	// No lease held: the fallback is silent (verify keeps the static path).
	e2 := &Executor{DHCPLeaseFor: func(string) net.IP { return nil }}
	if _, ok := e2.dhcpLeaseAddr(hw); ok {
		t.Fatal("fallback fired without a live lease")
	}

	// No hook (runner without the netboot facet): disabled.
	e3 := &Executor{}
	if _, ok := e3.dhcpLeaseAddr(hw); ok {
		t.Fatal("fallback fired without the lease hook")
	}
}
