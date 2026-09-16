package preseed

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
	// Pin the interface by MAC when the spec declares one: multi-port
	// machines make "auto" pick whichever link is up first — a port without
	// the provisioning L2 stalls netcfg's DHCP probe (real-hardware: 2288H
	// LOM port 2). d-i accepts a MAC as the choices value.
	if mac := interfaceMAC(entries); mac != "" {
		fmt.Fprintf(&b, "d-i netcfg/choose_interface select %s\n", strings.ToLower(mac))
	} else {
		b.WriteString("d-i netcfg/choose_interface select auto\n")
	}
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
			if isDefaultRoute(r.To) {
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
	if host, domain, ok := splitHostname(hostname); ok {
		fmt.Fprintf(&b, "d-i netcfg/get_hostname string %s\n", host)
		b.WriteString("d-i netcfg/get_hostname seen boolean true\n")
		if domain != "" {
			fmt.Fprintf(&b, "d-i netcfg/get_domain string %s\n", domain)
		}
	}
	return b.String(), nil
}

// interfaceMAC returns the MAC the spec pins the installer network to, if
// any — the first entry with a match.mac wins (the dialect's one-interface
// shape makes ordering moot).
func interfaceMAC(entries []render.NetworkEntry) string {
	for _, e := range entries {
		if e.Match != nil && e.Match.MAC != "" {
			return e.Match.MAC
		}
	}
	return ""
}

// netcfgKernelArgs mirrors netcfgSection as kernel-command-line preseed:
// a URL-loaded seed arrives AFTER netcfg has run (the download needs a
// working network), so on the netboot carrier every netcfg answer — the
// interface choice above all — must ride the kernel command line to take
// effect at all (the official network-preseed ordering rule). Values must
// stay space-free: they land in one command-line string.
func netcfgKernelArgs(entries []render.NetworkEntry, hostname string) string {
	args := []string{"netcfg/enable=true"}
	if mac := interfaceMAC(entries); mac != "" {
		args = append(args, "netcfg/choose_interface="+strings.ToLower(mac))
	} else {
		args = append(args, "netcfg/choose_interface=auto")
	}
	for _, e := range entries {
		if len(e.Addresses) == 0 {
			continue
		}
		ip, ipnet, err := net.ParseCIDR(e.Addresses[0])
		if err != nil {
			continue // render already rejects it in netcfgSection
		}
		args = append(args,
			"netcfg/disable_autoconfig=true",
			"netcfg/get_ipaddress="+ip.String(),
			"netcfg/get_netmask="+dottedMask(ipnet.Mask),
			"netcfg/confirm_static=true",
		)
		for _, r := range e.Routes {
			if isDefaultRoute(r.To) {
				args = append(args, "netcfg/get_gateway="+r.Via)
			}
		}
		if len(e.Nameservers) > 0 {
			args = append(args, "netcfg/get_nameservers="+strings.Join(e.Nameservers, ","))
		}
		break // the dialect's one-static-entry limit; netcfgSection validates
	}
	if host, domain, ok := splitHostname(hostname); ok {
		args = append(args, "netcfg/get_hostname="+host)
		if domain != "" {
			args = append(args, "netcfg/get_domain="+domain)
		}
	}
	return strings.Join(args, " ")
}

// splitHostname splits a dotted hostname; ok is false for empty input.
func splitHostname(hostname string) (host, domain string, ok bool) {
	if hostname == "" {
		return "", "", false
	}
	host, domain = hostname, ""
	if i := strings.IndexByte(hostname, '.'); i > 0 {
		host, domain = hostname[:i], hostname[i+1:]
	}
	return host, domain, true
}

// isDefaultRoute matches the default-route spellings the spec accepts —
// "default" and the CIDR forms (0.0.0.0/0, ::/0). Missing this match leaves
// netcfg without a gateway, and its static-confirmation asks for one
// (real-hardware: the installer stalled on the gateway dialog).
func isDefaultRoute(to string) bool {
	switch to {
	case "default", "0.0.0.0/0", "::/0":
		return true
	}
	return false
}

// dottedMask renders a CIDR mask as the dotted-quad netcfg expects.
func dottedMask(mask net.IPMask) string {
	if len(mask) != net.IPv4len {
		return "255.255.255.0" // netcfg is v4-shaped; degenerate masks fall back
	}
	return net.IP(mask).String()
}
