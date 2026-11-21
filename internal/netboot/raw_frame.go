package netboot

import (
	"encoding/binary"
	"net"
)

// Raw-frame parsing for the Linux AF_PACKET receive channel
// (rawrecv_linux.go): some PXE firmwares (Huawei iBMC observed on 2288H
// V5 LOM) emit DHCP DISCOVERs with an invalid UDP checksum, and the
// kernel silently drops those before any UDP socket can see them. The
// AF_PACKET socket sees every frame at L2 — ParseFrame is the filter that
// decides which frames the raw channel feeds to the DHCP handler or
// re-injects with a repaired checksum (TFTP sessions, FixChecksum).
//
// De-dup contract: frames whose UDP checksum VALIDATES are already
// delivered by the kernel to the ordinary UDP socket — the raw channel
// must skip them (checksumOK=true). Only frames that FAIL validation
// (checksumOK=false) are handed onward, so each datagram is processed
// exactly once regardless of how the client's firmware computes its
// checksums.

const udpProto = 17 // IP protocol number for UDP

// ParseFrame decodes an ethernet frame into ANY IPv4 UDP datagram. The raw
// channel (rawrecv_linux.go) decides by dstPort what to do with it: 67/4011
// go to the DHCP handler, ports registered by live TFTP sessions are
// re-injected with a repaired checksum, everything else is skipped. It
// returns ok=false for non-IPv4/UDP frames, and checksumOK=true for
// datagrams whose UDP checksum validates (the kernel's UDP socket already
// owns those). VLAN-tagged frames are not handled here — the AF_PACKET
// socket is opened with ETH_P_IP, which does not see 802.1Q frames.
func ParseFrame(frame []byte) (payload []byte, srcIP net.IP, dstPort int, checksumOK bool) {
	// Ethernet II header: dst(6) src(6) type(2).
	if len(frame) < 14 {
		return nil, nil, 0, false
	}
	ethertype := binary.BigEndian.Uint16(frame[12:14])
	payload = frame[14:]
	if ethertype == 0x8100 { // 802.1Q — not supported on this channel
		return nil, nil, 0, false
	}
	if ethertype != 0x0800 || len(payload) < 20 {
		return nil, nil, 0, false
	}
	if payload[0]>>4 != 4 {
		return nil, nil, 0, false
	}
	ihl := int(payload[0]&0x0f) * 4
	if ihl < 20 || len(payload) < ihl {
		return nil, nil, 0, false
	}
	if payload[9] != udpProto {
		return nil, nil, 0, false
	}
	srcIP = net.IP(payload[12:16])

	udp := payload[ihl:]
	if len(udp) < 8 {
		return nil, nil, 0, false
	}
	dstPort = int(binary.BigEndian.Uint16(udp[2:4]))
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < 8 || udpLen > len(udp) {
		return nil, nil, 0, false
	}
	dgram := udp[:udpLen] // UDP header + datagram

	wire := binary.BigEndian.Uint16(udp[6:8])
	// Zero checksum means "not computed" (IPv4) — the kernel accepts it and
	// so does the UDP path; the raw channel skips it either way.
	if wire == 0 {
		return dgram[8:], srcIP, dstPort, true
	}
	// Pseudo-header: src IP, dst IP, zeroed protocol byte + UDP length.
	pseudo := make([]byte, 0, 12+len(dgram))
	pseudo = append(pseudo, payload[12:16]...)
	pseudo = append(pseudo, payload[16:20]...)
	pseudo = append(pseudo, []byte{0, udpProto}...)
	pseudo = append(pseudo, udp[4:6]...)
	sum := rawChecksum(append(pseudo, dgram...))
	if sum != 0 {
		// Validation failed — the kernel's UDP socket silently dropped this
		// datagram; it is ours to handle.
		return dgram[8:], srcIP, dstPort, false
	}
	return dgram[8:], srcIP, dstPort, true
}

// FixChecksum rewrites the UDP checksum of an IPv4 datagram in place so the
// kernel accepts it. The raw channel uses it to re-inject checksum-broken
// datagrams (TFTP ACK/RRQ on port 69) to their local session socket, which
// the kernel had dropped. Returns false when the datagram is not a
// UDP-over-IPv4 packet this function can rewrite.
func FixChecksum(ipPkt []byte) bool {
	if len(ipPkt) < 28 || ipPkt[0]>>4 != 4 {
		return false
	}
	ihl := int(ipPkt[0]&0x0f) * 4
	if ihl < 20 || len(ipPkt) < ihl+8 || ipPkt[9] != udpProto {
		return false
	}
	udp := ipPkt[ihl:]
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < 8 || udpLen > len(udp) {
		return false
	}
	udp[6], udp[7] = 0, 0 // zero the field before computing
	pseudo := make([]byte, 0, 12+udpLen)
	pseudo = append(pseudo, ipPkt[12:16]...)
	pseudo = append(pseudo, ipPkt[16:20]...)
	pseudo = append(pseudo, []byte{0, udpProto}...)
	pseudo = append(pseudo, udp[4:6]...)
	c := rawChecksum(append(pseudo, udp[:udpLen]...))
	udp[6], udp[7] = byte(c>>8), byte(c)
	return true
}

// rawChecksum is the internet checksum over data (which already carries the
// pseudo-header prefix and a zeroed checksum field).
func rawChecksum(data []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(data); i += 2 {
		s += uint32(data[i])<<8 + uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		s += uint32(data[len(data)-1]) << 8
	}
	for s>>16 != 0 {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}
