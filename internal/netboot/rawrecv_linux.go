//go:build linux

// Raw receive channel for DHCP datagrams whose UDP checksum the kernel
// silently drops. Some PXE firmwares (Huawei iBMC on 2288H V5 LOM observed,
// 2026-11-21 install round) emit DISCOVERs with invalid UDP checksums; the
// kernel validates and discards them before any UDP socket sees the packet,
// so every Linux DHCP service answers nothing and the ROM times out with
// PXE-E18.
//
// An AF_PACKET socket sees frames at L2, before that validation. This
// channel feeds only frames whose checksum FAILS validation to the same
// handle the UDP sockets use — checksum-valid frames stay on the kernel
// path (see raw_frame.go's de-dup contract), so each datagram is processed
// exactly once. Replies ride the ordinary UDP sockets: the kernel computes
// a correct checksum on the way out, which is precisely what the broken
// firmware's DHCP client needs to see.
//
// Raw sockets require CAP_NET_RAW. Missing capability degrades to a
// warning — the UDP path keeps working for well-behaved clients (the
// syslog-sink degradation precedent: diagnostics, never a lifeline).

package netboot

import (
	"context"
	"errors"
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const rawEthPAll = 0x0003 // every ethernet frame type; the parse filters IP/UDP

// startRawRecv opens the AF_PACKET listener and spawns its read loop.
// Called after the UDP sockets are bound; a setup failure (missing
// CAP_NET_RAW is the usual shape) is the caller's warning, never fatal.
func (s *Server) startRawRecv(ctx context.Context, dhcpConn, proxyConn *net.UDPConn) error {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(rawEthPAll)))
	if err != nil {
		return fmt.Errorf("raw recv: AF_PACKET socket: %w", err)
	}
	// Bind any interface (Ifindex 0): DISCOVERs arrive on whichever
	// provisioning L2 this host sits on.
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(rawEthPAll), Ifindex: 0}); err != nil {
		unix.Close(fd)
		return fmt.Errorf("raw recv: bind: %w", err)
	}
	go s.serveRaw(ctx, fd, ipv4.NewPacketConn(dhcpConn), ipv4.NewPacketConn(proxyConn))
	return nil
}

func (s *Server) serveRaw(ctx context.Context, fd int, dhcpPC, proxyPC *ipv4.PacketConn) {
	defer unix.Close(fd)
	buf := make([]byte, 1500)
	for {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return // socket closed with the server
		}
		if ctx.Err() != nil {
			return
		}
		frame := buf[:n]
		payload, src, dstPort, needsHandling := ParseFrame(frame)
		if !needsHandling || len(payload) == 0 {
			continue
		}
		ifindex := 0
		if ll, ok := from.(*unix.SockaddrLinklayer); ok {
			if ll.Pkttype == unix.PACKET_OUTGOING {
				continue // our own replies echoed back by the kernel
			}
			ifindex = ll.Ifindex
		}
		// TFTP (69): the datagrams belong to live transfer sessions — their
		// session sockets are bound to this host and the kernel dropped the
		// datagrams for the broken checksum. Repair the checksum and
		// re-inject the datagram locally: the kernel delivers it to the
		// session socket, and the transfer resumes as if nothing happened.
		// (Re-injection must NOT change the source address — TFTP sessions
		// are keyed on the client's addr, and the raw frame carries it.)
		if dstPort == tftpPort {
			ipPkt := frame[14:]
			if FixChecksum(ipPkt) {
				dst := &unix.SockaddrInet4{Port: dstPort}
				copy(dst.Addr[:], frame[30:34]) // inner IP dst = this host
				if serr := unix.Sendto(fd, ipPkt, 0, dst); serr != nil {
					s.logf("tftp: raw re-inject to local session port %d: %v", dstPort, serr)
				}
			}
			continue
		}
		reply, to := s.handle(payload, dstPort, &net.UDPAddr{IP: src, Port: 68})
		if reply == nil || to == nil {
			continue
		}
		pc := dhcpPC
		if dstPort == proxyDHCPPort {
			pc = proxyPC
		}
		// Same visibility as the UDP path — the raw channel's handling must
		// be observable in the log like every other responder decision.
		ifindexOut := ifindex
		s.logf("dhcp: raw request from %s (ifindex %d) -> reply to %s", src, ifindexOut, to)
		// Reply through the ordinary UDP socket — the kernel computes the
		// UDP checksum on transmit, and forcing the egress interface to
		// the arrival one matches the UDP path's PKTINFO behaviour.
		if _, err := pc.WriteTo(reply, &ipv4.ControlMessage{IfIndex: ifindex}, to); err != nil {
			s.logf("dhcp: raw-channel reply to %s: %v", to, err)
		}
	}
}

// htons converts a host-order uint16 to network order (no-op byte swap).
func htons(v uint16) uint16 {
	return v<<8 | v>>8
}
