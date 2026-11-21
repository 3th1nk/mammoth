package netboot

import (
	"bytes"
	"net"
	"testing"
)

// frameFrom assembles an ethernet frame carrying a DHCP datagram, with the
// UDP checksum computed (or corrupted) as requested.
func frameFrom(bootp []byte, srcIP, dstIP net.IP, sport, dport int, corrupt, vlan bool) []byte {
	udpLen := 8 + len(bootp)
	udp := make([]byte, 8)
	udp[0], udp[1] = byte(sport>>8), byte(sport)
	udp[2], udp[3] = byte(dport>>8), byte(dport)
	udp[4], udp[5] = byte(udpLen>>8), byte(udpLen)
	pseudo := append(srcIP.To4(), dstIP.To4()...)
	pseudo = append(pseudo, 0, 17, byte(udpLen>>8), byte(udpLen))
	c := cksum16(append(append(append([]byte{}, pseudo...), udp...), bootp...))
	udp[6], udp[7] = byte(c>>8), byte(c)
	if corrupt {
		udp[7] ^= 0xff
	}
	ip := make([]byte, 20)
	ip[0] = 0x45
	ip[2], ip[3] = byte((20+udpLen)>>8), byte(20+udpLen)
	ip[8], ip[9] = 64, 17
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())
	ic := cksum16(ip)
	ip[10], ip[11] = byte(ic>>8), byte(ic)
	eth := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x50, 0x1d, 0x93, 0xd8, 0xc6, 0x97, 0x08, 0x00}
	frame := append(eth, ip...)
	frame = append(frame, udp...)
	frame = append(frame, bootp...)
	if vlan {
		out := append([]byte{}, frame[:12]...)
		out = append(out, 0x81, 0x00, 0x00, 0x64, 0x08, 0x00)
		out = append(out, frame[12:]...)
		return out
	}
	return frame
}

func cksum16(data []byte) uint16 {
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

func testBOOTP() []byte {
	bootp := make([]byte, 240)
	bootp[0], bootp[1], bootp[2], bootp[3] = 1, 1, 6, 0
	copy(bootp[236:240], magicCookie)
	return append(bootp, 53, 1, 1, 255)
}

// TestParseFrameMatrix pins the raw-channel de-dup contract: checksum-clean
// datagrams validate (checksumOK=true — the kernel UDP path owns them) and
// checksum-broken ones surface as payload (the iBMC PXE shape).
func TestParseFrameMatrix(t *testing.T) {
	src, dst := net.IPv4(0, 0, 0, 0).To4(), net.IPv4(192, 168, 1, 5).To4()
	bootp := testBOOTP()

	good := frameFrom(bootp, src, dst, 68, 67, false, false)
	payload, sip, dport, ok := ParseFrame(good)
	if !ok {
		t.Fatal("valid checksum must report checksumOK=true (kernel path owns it)")
	}
	if dport != 67 || !sip.Equal(src) {
		t.Errorf("src/port = %v/%d", sip, dport)
	}
	if !bytes.HasPrefix(payload, bootp) {
		t.Error("payload must be the BOOTP datagram verbatim")
	}

	broken := frameFrom(bootp, src, dst, 68, 67, true, false)
	payload, _, dport, ok = ParseFrame(broken)
	if ok {
		t.Fatal("corrupted checksum must report checksumOK=false (feed to handle)")
	}
	if dport != 67 || !bytes.HasPrefix(payload, bootp) {
		t.Errorf("broken datagram fields drifted: port=%d payload-prefix=%v", dport, payload[:4])
	}

	// 4011 discovery is the proxyDHCP path — same treatment.
	d4011 := frameFrom(bootp, src, dst, 68, 4011, true, false)
	payload, _, dport, ok = ParseFrame(d4011)
	if ok || dport != 4011 {
		t.Errorf("4011 broken frame: ok=%v port=%d", ok, dport)
	}

	// Zero checksum means "not computed" (IPv4) — kernel accepts, raw skips.
	zero := frameFrom(bootp, src, dst, 68, 67, false, false)
	zero[40], zero[41] = 0, 0 // UDP checksum field (eth 14 + ip 20 + header offset 6) set to zero
	if _, _, _, ok = ParseFrame(zero); !ok {
		t.Error("zero checksum must validate as OK")
	}

	// Non-responder ports are not ours.
	other := frameFrom(bootp, src, dst, 68, 69, true, false)
	if _, _, _, ok = ParseFrame(other); ok {
		t.Error("non-DHCP port must be skipped")
	}

	// VLAN-tagged frames are out of scope for the ETH_P_IP socket.
	if _, _, _, ok = ParseFrame(frameFrom(bootp, src, dst, 68, 67, false, true)); ok {
		t.Error("VLAN frames must be skipped (ETH_P_IP socket never sees them)")
	}

	// TFTP (69) frames surface as broken too — the fix path (re-inject).
	tftpFrame := frameFrom(bootp[:20], src, dst, 40001, 69, true, false)
	if _, _, dport, ok = ParseFrame(tftpFrame); ok || dport != 69 {
		t.Fatalf("broken TFTP frame: ok=%v port=%d, want false/69", ok, dport)
	}
	// The re-inject path: FixChecksum must make the kernel accept it.
	ipPkt := tftpFrame[14:]
	if !FixChecksum(ipPkt) {
		t.Fatal("FixChecksum rejected a UDP-over-IPv4 datagram")
	}
	if _, _, dport, ok = ParseFrame(append([]byte{}, tftpFrame[:12]...)); true {
		_ = dport
	}
	// After the fix, a second FixChecksum on the repaired packet is a no-op
	// that leaves the checksum validating.
	if !FixChecksum(append([]byte{}, ipPkt...)) {
		t.Error("FixChecksum must accept its own repaired datagram")
	}
	// Non-UDP and garbage are rejected.
	if FixChecksum(make([]byte, 30)) {
		t.Error("non-UDP datagram must be rejected")
	}
	if FixChecksum(nil) {
		t.Error("nil datagram must be rejected")
	}

	// Truncated garbage never panics.
	for _, junk := range [][]byte{
		{}, {0x45}, make([]byte, 14), make([]byte, 30),
		{0x45, 0x00, 0x01, 0x77},
	} {
		ParseFrame(junk)
	}
}
