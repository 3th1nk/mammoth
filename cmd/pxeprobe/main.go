// Command pxeprobe exercises a running mammoth netboot service the way a
// PXE firmware + iPXE would: DHCP DISCOVER (unicast) → TFTP RRQ of the
// offered NBP → HTTP fetch of the offered boot file. Field triage tool:
// when a machine stalls at the PXE prompt, run this from any host on the
// provisioning L2 and read which hop dies. It never assigns an address and
// never arms entries — pure observer.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	opRequest, opReply  = 1, 2
	optMessageType      = 53
	optVendorClass      = 60
	optTFTPServer       = 66
	optBootfile         = 67
	optUserClass        = 77
	optArch             = 93
	optFeatures         = 175
	msgDiscover         = 1
	cookie              = "\x63\x82\x53\x63"
)

func main() {
	server := flag.String("server", "", "netboot host (IP) running proxyDHCP:67/69")
	mac := flag.String("mac", "52:54:00:12:34:56", "client MAC to present")
	arch := flag.Uint("arch", 0, "DHCP option 93 arch code: 0=bios 6=ia32 7=x64 9=arm64")
	ipxe := flag.Bool("ipxe", false, "pretend to already run iPXE (user-class + opt 175)")
	flag.Parse()
	if *server == "" {
		fmt.Fprintln(os.Stderr, "usage: pxeprobe -server <ip> [-mac …] [-arch N] [-ipxe]")
		os.Exit(2)
	}

	req := discover(*mac, uint16(*arch), *ipxe)
	offer, err := dhcpExchange(net.ParseIP(*server), req)
	fatal(err)
	printOffer(offer)

	file := bootfileOf(offer)
	switch {
	case strings.HasPrefix(file, "http://"), strings.HasPrefix(file, "https://"):
		fmt.Printf("hop3 iPXE script %s\n", file)
		resp, err := http.Get(file)
		fatal(err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("      HTTP %d, %d bytes:\n%s\n", resp.StatusCode, len(body), body)
	case file != "":
		fmt.Printf("hop2 TFTP RRQ %s from %s:69\n", file, *server)
		n, err := tftpProbe(net.ParseIP(*server), file)
		fatal(err)
		fmt.Printf("      NBP served, first data block %d bytes ✓\n", n)
	default:
		fmt.Println("      (no bootfile offered — silent fall-through path)")
	}
}

// discover builds a minimal DHCPDISCOVER with PXEClient vendor class.
func discover(macStr string, arch uint16, isIPXE bool) []byte {
	var mac [6]byte
	fmt.Sscanf(strings.NewReplacer(":", "", "-", "").Replace(macStr), "%2x%2x%2x%2x%2x%2x",
		&mac[0], &mac[1], &mac[2], &mac[3], &mac[4], &mac[5])
	b := make([]byte, 240)
	b[0], b[1], b[2] = opRequest, 1, 6
	binary.BigEndian.PutUint32(b[4:8], 0x12345678)
	copy(b[28:34], mac[:])
	copy(b[236:240], cookie)
	put := func(code byte, data []byte) {
		b = append(b, code, byte(len(data)))
		b = append(b, data...)
	}
	put(optMessageType, []byte{msgDiscover})
	put(optVendorClass, []byte(fmt.Sprintf("PXEClient:Arch:%05X:UNDI:003016", arch)))
	ab := make([]byte, 2)
	binary.BigEndian.PutUint16(ab, arch)
	put(optArch, ab)
	if isIPXE {
		put(optUserClass, append([]byte{4}, "iPXE"...))
		put(optFeatures, []byte{0x01, 0x01})
	}
	return append(b, 255)
}

// dhcpExchange sends one unicast request and waits for a BOOTREPLY.
func dhcpExchange(serverIP net.IP, req []byte) ([]byte, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.WriteToUDP(req, &net.UDPAddr{IP: serverIP, Port: 67}); err != nil {
		return nil, err
	}
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 1500)
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return nil, fmt.Errorf("no DHCP reply in 4s (hop1 dead: DISCOVER reached :67?)")
		}
		if n >= 240 && string(buf[236:240]) == cookie && buf[0] == opReply {
			return buf[:n], nil
		}
	}
}

func printOffer(p []byte) {
	if mt := byteAt(p, optMessageType); mt != 0 {
		fmt.Printf("hop1 DHCP reply type=%d (2=OFFER 5=ACK)\n", mt)
	}
	fmt.Printf("      siaddr=%s tftp=%q bootfile=%q\n",
		net.IP(p[20:24]), cstr(p, optTFTPServer), cstr(p, optBootfile))
}

func bootfileOf(p []byte) string {
	if f := cstr(p, optBootfile); f != "" {
		return f
	}
	// Some ROMs read only the BOOTP file field.
	for i := 108; i < 236 && p[i] != 0; i++ {
		if i == 108 && p[108] == 0 {
			break
		}
	}
	if p[108] != 0 {
		e := 108
		for e < 236 && p[e] != 0 {
			e++
		}
		return string(p[108:e])
	}
	return ""
}

// tftpProbe performs an RRQ and waits for the first data block.
func tftpProbe(serverIP net.IP, name string) (int, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return 0, err
	}
	defer c.Close()
	rrq := append([]byte{0, 1}, []byte(name+"\x00octet\x00blksize\x001428\x00tsize\x000\x00")...)
	if _, err := c.WriteToUDP(rrq, &net.UDPAddr{IP: serverIP, Port: 69}); err != nil {
		return 0, err
	}
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return 0, fmt.Errorf("no TFTP reply in 4s (hop2 dead: RRQ reached :69?)")
		}
		if n < 4 {
			continue
		}
		op := binary.BigEndian.Uint16(buf[:2])
		if op == 6 { // OACK — ack it, data follows
			ack := []byte{0, 4, 0, 0}
			c.WriteToUDP(ack, from)
			continue
		}
		if op == 3 {
			return n - 4, nil
		}
		if op == 5 {
			return 0, fmt.Errorf("TFTP error %d: %s", binary.BigEndian.Uint16(buf[2:4]), buf[4:n])
		}
	}
}

func byteAt(p []byte, code byte) byte {
	for i := 240; i+1 < len(p); {
		if p[i] == 0 {
			i++
			continue
		}
		if p[i] == 255 {
			break
		}
		l := int(p[i+1])
		if i+2+l > len(p) {
			break
		}
		if p[i] == code {
			return p[i+2]
		}
		i += 2 + l
	}
	return 0
}

func cstr(p []byte, code byte) string {
	for i := 240; i+1 < len(p); {
		if p[i] == 0 {
			i++
			continue
		}
		if p[i] == 255 {
			break
		}
		l := int(p[i+1])
		if i+2+l > len(p) {
			break
		}
		if p[i] == code {
			return strings.TrimRight(string(p[i+2:i+2+l]), "\x00")
		}
		i += 2 + l
	}
	return ""
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "pxeprobe:", err)
		os.Exit(1)
	}
}
