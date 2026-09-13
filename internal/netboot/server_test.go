package netboot

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// freeUDPPort grabs an ephemeral port number for the server to bind.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// TestStartEndToEnd drives the real Server sockets on unprivileged ports:
// proxyDHCP answers a PXE ROM and an iPXE client; TFTP serves the NBP.
func TestStartEndToEnd(t *testing.T) {
	dhcp := freeUDPPort(t)
	proxy := freeUDPPort(t)
	tftp := freeUDPPort(t)
	nbps := fstest.MapFS{
		"undionly.kpxe":  &fstest.MapFile{Data: []byte("kpxe-bytes")},
		"ipxe-amd64.efi": &fstest.MapFile{Data: []byte("efi-bytes")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Start(ctx, Options{
		DHCPPort:   dhcp,
		ProxyPort:  proxy,
		TFTPPort:   tftp,
		NextServer: []byte{127, 0, 0, 1},
		BaseURL:    fmt.Sprintf("http://127.0.0.1:8080"),
		NBPs:       nbps,
		Log:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	c := startClient(t)
	var mac [6]byte
	copy(mac[:], []byte{0x52, 0x54, 0x00, 0xab, 0xcd, 0xef})

	// Plain ROM discover on the DHCP port.
	req := discover(42, mac,
		option{optVendorClass, []byte("PXEClient:Arch:00000:UNDI:002001")},
		option{optArch, u16opt(0)},
	)
	c.send(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: dhcp}, req)
	reply := c.recv()
	p, perr := parse(reply)
	if perr != nil {
		t.Fatalf("reply: %v", perr)
	}
	if got := hexClean(p.file[:]); got != "undionly.kpxe" {
		t.Fatalf("bootfile %q", got)
	}

	// iPXE discover on the boot-server discovery port.
	req = discover(43, mac,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003019")},
		option{optUserClass, []byte{4, 'i', 'P', 'X', 'E'}},
	)
	c.send(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: proxy}, req)
	reply = c.recv()
	p, _ = parse(reply)
	if !strings.Contains(string(p.options[optBootfile]), "/netboot/script?mac=52:54:00:ab:cd:ef") {
		t.Fatalf("script url: %q", p.options[optBootfile])
	}

	// TFTP actually serves the embedded-style file map.
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tftp}
	got := c.fetch(addr, "ipxe-amd64.efi")
	if string(got) != "efi-bytes" {
		t.Fatalf("tftp got %q", got)
	}

	// Shutdown: Wait unblocks with nil.
	cancel()
	select {
	case err := <-doneOf(s):
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestStartValidation(t *testing.T) {
	// Non-IPv4 NextServer is a config error, fail before touching ports.
	if _, err := Start(context.Background(), Options{
		NextServer: []byte("not-an-ip"),
		NBPs:       fstest.MapFS{},
	}); err == nil {
		t.Fatal("want error for non-IPv4 NextServer")
	}
}

func doneOf(s *Server) <-chan error { return s.done }
