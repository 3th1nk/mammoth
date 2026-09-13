package netboot

import "net"

// NBP file names inside the embedded NBPs filesystem (assets/pxe).
const (
	nbpBIOS  = "undionly.kpxe"  // BIOS chainloader, rides the NIC's UNDI ROM
	nbpX64   = "ipxe-amd64.efi" // UEFI x64 full iPXE
	nbpARM64 = "ipxe-arm64.efi" // UEFI aarch64
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
// request datagram received on dhcpPort or proxyPort, return the reply to
// send back to its source, or nil to stay silent. Staying silent is the
// normal behavior for everything that is not our business (site DHCP
// clients, other vendors' PXE, unknown architectures) — the firmware then
// falls through to its next boot device on its own.
//
// proxyDHCP semantics (Pixiecore model): the site DHCP server assigns the
// address; we only tell PXE clients where the next boot program lives.
// yiaddr stays 0.0.0.0 in every reply, and we never ACK a REQUEST that
// selected a different server identifier.
func (s *Server) handle(req []byte, port int) []byte {
	p, err := parse(req)
	if err != nil {
		return nil
	}
	if p.op != opRequest || !p.isPXEClient() {
		return nil
	}
	switch p.messageType() {
	case msgDiscover:
		// proceed
	case msgRequest:
		if port == s.opts.DHCPPort {
			// On the DHCP port a REQUEST that selected another server (the
			// site DHCP, by server identifier) is not ours to answer.
			if sid, ok := p.options[optServerID]; ok && !equalIP(sid, s.opts.NextServer) {
				return nil
			}
		}
	default:
		return nil // RELEASE/INFORM/DECLINE: never our business
	}
	return s.reply(p)
}

// reply builds the boot-information reply for one PXE client.
func (s *Server) reply(p *packet) []byte {
	mac := p.mac()
	if mac == "" {
		return nil
	}
	arch, ok := p.arch()
	if !ok {
		s.logf("dhcp: pxe client without recognizable arch (mac %s) — silent", mac)
		return nil
	}

	msgType := byte(msgOffer)
	if p.messageType() == msgRequest {
		msgType = msgAck
	}
	next := s.opts.NextServer.To4()

	// iPXE identifies itself and accepts a full URL in the bootfile slot:
	// straight to the per-MAC script over HTTP, skipping the TFTP hop.
	if p.isIPXE() {
		return p.bytes(msgType, scriptURLFor(s.opts.BaseURL, mac, arch),
			option{optServerID, next},
		)
	}

	name := nbpFor(arch)
	if name == "" || !s.hasNBP(name) {
		s.logf("dhcp: no boot program for arch %s (mac %s) — silent", arch, mac)
		return nil
	}
	// Plain PXE ROMs want a bare file name plus the TFTP server address;
	// option 66 is a string by spec, siaddr (the BOOTP field) carries the
	// same IP numerically for ROMs that only read that.
	return p.bytes(msgType, name,
		option{optServerID, next},
		option{optTFTPServer, []byte(s.opts.NextServer.String())},
	)
}

// equalIP compares an option payload against a 4-byte IPv4 address.
func equalIP(b []byte, ip net.IP) bool {
	v4 := ip.To4()
	return len(b) == 4 && v4 != nil && string(b) == string(v4)
}
