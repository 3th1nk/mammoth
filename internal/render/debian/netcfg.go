package debian

import (
	"fmt"
	"net"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// netcfgSection renders the network preseed block. d-i's netcfg is the
// dialect's narrowest dimension: ONE interface, no bond/vlan, and no MAC
// matching — the static values land on whichever interface netcfg picks.
// The install-time address is what matters (the completion callback rides it).
func netcfgSection(distro string, entries []render.NetworkEntry, hostname string) (string, error) {
	prefix := distro + ": "

	static := []render.NetworkEntry{}
	for _, e := range entries {
		switch {
		case e.Bond != nil:
			return "", fmt.Errorf("%snetwork bonding is not supported by netcfg (debian-installer); declare plain interfaces", prefix)
		case e.VLAN != nil:
			return "", fmt.Errorf("%snetwork vlan is not supported by netcfg (debian-installer); declare plain interfaces", prefix)
		}
		if len(e.Addresses) > 0 {
			static = append(static, e)
		}
	}
	if len(static) > 1 {
		return "", fmt.Errorf("%sonly one static network entry is supported (netcfg configures one interface); %d declared", prefix, len(static))
	}

	var b strings.Builder
	b.WriteString("d-i netcfg/enable boolean true\n")
	b.WriteString("d-i netcfg/choose_interface select auto\n")
	if len(static) == 0 {
		b.WriteString("d-i netcfg/dhcp_timeout string 10\n")
		b.WriteString("d-i netcfg/dhcpv6_timeout string 10\n")
	} else {
		// The static entry drives the installer network (netcfg cannot pin it
		// to a MAC — the first wired interface wins).
		e := static[0]
		ip, ipnet, err := net.ParseCIDR(e.Addresses[0])
		if err != nil {
			return "", fmt.Errorf("%sinvalid network address %q", prefix, e.Addresses[0])
		}
		// Skip the DHCP probe entirely: on a network without a DHCP server
		// netcfg raises "Network autoconfiguration failed" (an error-level
		// dialog that autoinstall's critical priority still displays) before
		// it ever reaches the static values (official static-preseed shape).
		b.WriteString("d-i netcfg/disable_autoconfig boolean true\n")
		b.WriteString("d-i netcfg/dhcp_options select Configure network manually\n")
		fmt.Fprintf(&b, "d-i netcfg/get_ipaddress string %s\n", ip.String())
		fmt.Fprintf(&b, "d-i netcfg/get_netmask string %s\n", dottedMask(ipnet.Mask))
		for _, r := range e.Routes {
			if r.To == "default" {
				fmt.Fprintf(&b, "d-i netcfg/get_gateway string %s\n", r.Via)
			}
		}
		if len(e.Nameservers) > 0 {
			fmt.Fprintf(&b, "d-i netcfg/get_nameservers string %s\n", strings.Join(e.Nameservers, " "))
		}
		if len(e.Search) > 0 {
			fmt.Fprintf(&b, "d-i netcfg/get_searchdomains string %s\n", strings.Join(e.Search, " "))
		}
		b.WriteString("d-i netcfg/confirm_static boolean true\n")
	}

	// Identity: netcfg asks for the hostname right after link setup, so it is
	// pinned here (seen=true — netcfg would otherwise prefer a DHCP-provided
	// name). A dotted hostname splits into host + domain.
	if hostname != "" {
		host, domain := hostname, ""
		if i := strings.IndexByte(hostname, '.'); i > 0 {
			host, domain = hostname[:i], hostname[i+1:]
		}
		fmt.Fprintf(&b, "d-i netcfg/get_hostname string %s\n", host)
		b.WriteString("d-i netcfg/get_hostname seen boolean true\n")
		if domain != "" {
			fmt.Fprintf(&b, "d-i netcfg/get_domain string %s\n", domain)
		}
	}
	return b.String(), nil
}

// dottedMask renders a CIDR mask as the dotted-quad netcfg expects.
func dottedMask(mask net.IPMask) string {
	if len(mask) != net.IPv4len {
		return "255.255.255.0" // netcfg is v4-shaped; degenerate masks fall back
	}
	return net.IP(mask).String()
}
