package render

import (
	"fmt"
	"net"
	"strings"
)

// EarlyNetArgs renders dracut/initramfs early-network kernel arguments from
// the spec's static interface declarations. The remote answer file is a URL
// on the boot media's command line — the installer must have network up
// BEFORE it can fetch it, and without an ip= argument it configures none
// (real-hardware finding: the installer sat idle and never fetched the
// kickstart). pinIfname additionally renames interfaces by MAC (dracut
// ifname=) for distros whose early boot honors it.
//
// Bond/vlan entries never contribute: their stanza generation belongs to
// the answer file; the early phase just needs one reachable path.
func EarlyNetArgs(entries []NetworkEntry, pinIfname bool) string {
	var b strings.Builder
	seenDNS := map[string]bool{}
	n := 0
	for _, e := range entries {
		if e.Bond != nil || e.VLAN != nil || e.Match == nil || e.Match.MAC == "" || len(e.Addresses) == 0 {
			continue
		}
		ipAddr, ipnet, err := net.ParseCIDR(e.Addresses[0])
		if err != nil || ipAddr.To4() == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		mask := net.IP(net.CIDRMask(ones, 32)).String()

		var iface string
		if pinIfname {
			iface = fmt.Sprintf("m%d", n)
			fmt.Fprintf(&b, " ifname=%s:%s", iface, strings.ToLower(e.Match.MAC))
		}
		n++

		gateway := ""
		for _, r := range e.Routes {
			if r.To == "0.0.0.0/0" || r.To == "default" || r.To == "::/0" {
				gateway = r.Via
			}
		}
		fmt.Fprintf(&b, " ip=%s::%s:%s::%s:none", ipAddr.String(), gateway, mask, iface)
		for _, dns := range e.Nameservers {
			if !seenDNS[dns] {
				seenDNS[dns] = true
				fmt.Fprintf(&b, " nameserver=%s", dns)
			}
		}
	}
	return strings.TrimSpace(b.String())
}
