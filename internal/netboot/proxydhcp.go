package netboot

import (
	"context"
	"encoding/binary"
	"net"
	"time"
)

// NBP file names inside the embedded NBPs filesystem (assets/pxe).
const (
	nbpBIOS     = "undionly.kpxe"  // BIOS chainloader, rides the NIC's UNDI ROM
	nbpX64      = "shimx64.efi"    // UEFI x64 Secure Boot chain: shim (Microsoft-signed) loads grubx64.efi (Debian-signed grubnet)
	nbpX64Plain = "ipxe-amd64.efi" // UEFI x64 without Secure Boot: plain iPXE, the wimboot carrier's host
	nbpARM64    = "shimaa64.efi"   // UEFI aarch64 Secure Boot chain: same shape as x64 — shim (Microsoft-signed) loads grubaa64.efi (Debian-signed grubnetaa64)
)

// grubX64 and grubARM64 are the shim second stages: the firmware loads
// shimx64.efi / shimaa64.efi, which fetch grubx64.efi / grubaa64.efi from
// the same TFTP directory by convention. They are served from the same
// embedded FS but never named as the DHCP bootfile.
const (
	grubX64   = "grubx64.efi"
	grubARM64 = "grubaa64.efi"
)

// nbpFor maps a firmware architecture to its network boot program.
func nbpFor(a Arch) string {
	switch a {
	case ArchBIOS:
		return nbpBIOS
	case ArchX64:
		return nbpX64
	case ArchARM64:
		return nbpARM64
	default:
		return "" // ia32 and unknown: unsupported — silence falls through
	}
}

// handle is the pure decision core of the proxyDHCP responder: given a
// request datagram received on dhcpPort or proxyPort, return the reply and
// the address to send it to, or nil to stay silent. Staying silent is the
// normal behavior for everything that is not our business (site DHCP
// clients, other vendors' PXE, unknown architectures) — the firmware then
// falls through to its next boot device on its own.
//
// proxyDHCP semantics (Pixiecore model): the site DHCP server assigns the
// address; we only tell PXE clients where the next boot program lives.
// yiaddr stays 0.0.0.0 in every reply, and we never ACK a REQUEST that
// selected a different server identifier.
func (s *Server) handle(req []byte, port int, src *net.UDPAddr) ([]byte, *net.UDPAddr) {
	p, err := parse(req)
	if err != nil {
		return nil, nil
	}
	if p.op != opRequest {
		return nil, nil
	}
	// Proxy mode answers PXE clients only. Pool mode is the address
	// authority for the whole L2 (docs/operations.md §4.5): the installer
	// kernel's own DHCP (dracut ip=dhcp) carries no option 60 and must be
	// served too — it retries forever otherwise (2288H finding).
	if !p.isPXEClient() && s.opts.DHCP == nil {
		return nil, nil
	}
	switch p.messageType() {
	case msgDiscover:
		// proceed
	case msgRequest:
		if port == s.opts.DHCPPort {
			// A REQUEST that selected another server (by server identifier)
			// is not ours to answer.
			if sid, ok := p.options[optServerID]; ok && !equalIP(sid, s.opts.NextServer) {
				return nil, nil
			}
		}
	default:
		return nil, nil // RELEASE/INFORM/DECLINE: never our business
	}
	reply := s.reply(p)
	if reply == nil {
		return nil, nil
	}
	return reply, bootReplyAddr(p, src, s.opts.broadcastFor())
}

// bootReplyAddr selects where a reply goes (RFC 2131 §4.1): a boot ROM has
// no address yet — it broadcasts from 0.0.0.0 and (with the broadcast flag
// set) can only receive broadcast replies. Replying to the packet's source
// address there would send the offer to 0.0.0.0, which the kernel quietly
// delivers to loopback — the offer vanishes without a single error log
// (the 2288H real-hardware finding that cost the first PXE boot attempt).
func bootReplyAddr(p *packet, src *net.UDPAddr, broadcastAddr net.IP) *net.UDPAddr {
	port := 68
	if src != nil && src.Port != 0 {
		port = src.Port
	}
	// Relayed request (giaddr set, RFC 2131 §4.1): the reply goes to the
	// relay agent at giaddr:67, which forwards it onto the client's L2.
	// Without this branch a relayed DISCOVER carrying the broadcast flag
	// would be answered on mammoth's own L2 — the relay never sees it and
	// the client strands. Single-L2 deployments have giaddr zero and never
	// take this branch.
	if g := net.IP(p.giaddr[:]).To4(); g != nil && !g.IsUnspecified() {
		return &net.UDPAddr{IP: g, Port: 67}
	}
	if src == nil || src.IP == nil || src.IP.IsUnspecified() || p.flags&0x8000 != 0 {
		// Limited broadcast (255.255.255.255) is routed by Linux out the
		// arriving interface, but macOS sends it via the default route —
		// unreachable for guests behind a local vmnet bridge. A configured
		// directed broadcast address (the provisioning subnet's
		// 192.168.x.255) routes correctly there instead.
		if broadcastAddr != nil {
			return &net.UDPAddr{IP: broadcastAddr, Port: port}
		}
		return &net.UDPAddr{IP: net.IPv4bcast, Port: port}
	}
	return src
}

// reply builds the boot-information reply for one PXE client.
func (s *Server) reply(p *packet) []byte {
	mac := p.mac()
	if mac == "" {
		return nil
	}
	msgType := byte(msgOffer)
	if p.messageType() == msgRequest {
		msgType = msgAck
	}
	next := s.opts.NextServer.To4()

	// siaddr (the BOOTP field) carries the next-server IP for ROMs that
	// read only the field and never the options.
	copy(p.siaddr[:], next)

	// Pool mode (MAMMOTH_PXE_DHCP_POOL): the ROM needs an IP lease before
	// it will fetch anything, and the installer kernel's dracut re-requests
	// one without any PXE options. Leases go ONLY to MACs with an armed
	// netboot entry — the pool is the address authority for the machines it
	// installs, not for the wire: foreign DHCP clients (site VMs, stray
	// servers) otherwise drain small pools mid-install (2288H machine room).
	var leaseOpts []option
	if s.opts.DHCP != nil {
		if s.opts.Resolver != nil {
			// The cached wrapper returns (nil, nil) for unknown MACs — both
			// must gate, or every foreign client passes the allowlist
			// (2288H machine room: VMs drained the pool through this hole).
			if e, err := s.opts.Resolver.Entry(context.Background(), mac); err != nil || e == nil {
				s.logf("dhcp: no armed entry for %s — not leasing (pool serves installs only)", mac)
				return nil
			}
		}
		if ip := s.opts.DHCP.lease(mac); ip != nil {
			copy(p.yiaddr[:], ip.To4())
			lb := make([]byte, 4)
			binary.BigEndian.PutUint32(lb, uint32(dhcpLeaseTTL/time.Second))
			leaseOpts = append(leaseOpts,
				option{optLeaseTime, lb},
				option{optRenewalTime, lb2(uint32(dhcpLeaseTTL / (2 * time.Second)))},
				option{optRebindingTime, lb2(uint32(dhcpLeaseTTL * 7 / (8 * time.Second)))},
				option{optSubnetMask, s.opts.DHCP.Mask().To4()},
			)
			if r := s.opts.DHCP.Router(); len(r.To4()) == 4 {
				leaseOpts = append(leaseOpts, option{optRouter, r.To4()})
			}
		} else {
			s.logf("dhcp: address pool exhausted (mac %s) — silent", mac)
			return nil
		}
	}

	if !p.isPXEClient() {
		// Plain host (installer kernel renewing its address): lease only —
		// never boot parameters, or random devices would chainload iPXE.
		return p.bytes(msgType, "", append(leaseOpts, option{optServerID, next})...)
	}

	arch, ok := p.arch()
	if !ok {
		s.logf("dhcp: pxe client without recognizable arch (mac %s) — silent", mac)
		return nil
	}
	if s.opts.OnObserve != nil {
		s.opts.OnObserve(mac, arch)
	}

	// Options the boot ROM checks before it will accept the offer: the
	// PXEClient vendor class marks a PXE-capable server, and the client
	// machine identifier (opt 97 GUID, RFC 4578) must be echoed verbatim —
	// Intel UEFI PXE treats a mismatched or missing echo as a non-PXE server.
	// (Real-hardware: the 2288H's UEFI PXE silently dropped offers without
	// these and fell through to disk.)
	opts := append(leaseOpts,
		option{optVendorClass, []byte("PXEClient")},
		option{optServerID, next},
	)
	if guid, ok := p.options[optClientArchGUID]; ok {
		opts = append(opts, option{optClientArchGUID, guid})
	}

	// iPXE identifies itself and accepts a full URL in the bootfile slot:
	// straight to the per-MAC script over HTTP, skipping the TFTP hop.
	if p.isIPXE() {
		return p.bytes(msgType, scriptURLFor(s.opts.BaseURL, mac, arch), opts...)
	}

	name := nbpFor(arch)
	// The Windows carrier rides iPXE — the only documented wimboot host — so
	// an x64 client whose armed entry is a wimboot tree gets the unsigned
	// ipxe.efi instead of the Secure Boot chain (shim→grubnet has no wimboot
	// path: grub's UEFI loaders cannot hand files to a chainloaded image).
	// The firmware must therefore run with Secure Boot off; a deployment can
	// MOK-enroll iPXE itself, which is deployment policy mammoth does not own
	// (docs/compat/distros.md §windows).
	if arch == ArchX64 && s.opts.Resolver != nil {
		if e, err := s.opts.Resolver.Entry(context.Background(), mac); err == nil && e != nil && e.Kernel == "wimboot" {
			name = nbpX64Plain
		}
	}
	if name == "" || !s.hasNBP(name) {
		s.logf("dhcp: no boot program for arch %s (mac %s) — silent", arch, mac)
		return nil
	}
	// Plain PXE ROMs want a bare file name plus the TFTP server address;
	// option 66 is a string by spec, siaddr carries the same IP numerically.
	return p.bytes(msgType, name, append(opts,
		option{optTFTPServer, []byte(s.opts.NextServer.String())},
	)...)
}

// lb2 packs a uint32 DHCP option value.
func lb2(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// equalIP compares an option payload against a 4-byte IPv4 address.
func equalIP(b []byte, ip net.IP) bool {
	v4 := ip.To4()
	return len(b) == 4 && v4 != nil && string(b) == string(v4)
}
