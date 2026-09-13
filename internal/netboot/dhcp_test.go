package netboot

import (
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"
)

// discover builds a minimal DHCPDISCOVER datagram for tests.
func discover(xid uint32, mac [6]byte, opts ...option) []byte {
	p := &packet{
		op:      opRequest,
		htype:   htypeEthernet,
		hlen:    hlenEthernet,
		xid:     xid,
		options: map[byte][]byte{},
	}
	copy(p.chaddr[:], mac[:])
	out := make([]byte, minPacketLen)
	out[0] = p.op
	out[1] = p.htype
	out[2] = p.hlen
	binary.BigEndian.PutUint32(out[4:8], p.xid)
	copy(out[28:44], p.chaddr[:])
	copy(out[236:240], magicCookie)
	put := func(code byte, data []byte) {
		out = append(out, code, byte(len(data)))
		out = append(out, data...)
	}
	put(optMessageType, []byte{msgDiscover})
	for _, o := range opts {
		put(o.code, o.data)
	}
	return append(out, optEnd)
}

func u16opt(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func TestParseRoundTrip(t *testing.T) {
	var mac [6]byte
	mac[0], mac[1], mac[5] = 0x52, 0x54, 0x56
	req := discover(0xdeadbeef, mac,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
		option{optUserClass, []byte{4, 'i', 'P', 'X', 'E'}},
	)
	p, err := parse(req)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.xid != 0xdeadbeef || p.mac() != "52:54:00:00:00:56" {
		t.Fatalf("identity lost: xid=%x mac=%s", p.xid, p.mac())
	}
	if !p.isPXEClient() || !p.isIPXE() {
		t.Fatal("PXEClient/iPXE not recognized")
	}
	if a, ok := p.arch(); !ok || a != ArchX64 {
		t.Fatalf("arch = %q %v, want uefi-x64", a, ok)
	}
	if p.messageType() != msgDiscover {
		t.Fatalf("message type = %d", p.messageType())
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string][]byte{
		"short":     []byte{opRequest, 1, 6},
		"no cookie": make([]byte, 260),
		"reply op":  func() []byte { b := make([]byte, 260); b[0] = opReply; return b }(),
	}
	for name, b := range cases {
		if _, err := parse(b); err == nil {
			t.Errorf("%s: want ErrNotDHCP, got nil", name)
		}
	}
	// A truncated option tail is tolerated (interop over strictness): the
	// datagram still parses, the bad option is simply absent.
	p, err := parse(append(discover(1, [6]byte{}), optServerID, 40, 1, 2, 3))
	if err != nil {
		t.Fatalf("truncated tail must parse: %v", err)
	}
	if _, present := p.options[optServerID]; present {
		t.Error("truncated option must not be exposed")
	}
}

func TestArchDecoding(t *testing.T) {
	cases := []struct {
		name string
		v    []byte
		want Arch
		ok   bool
	}{
		{"bios", u16opt(0), ArchBIOS, true},
		{"ia32", u16opt(6), ArchIA32, true},
		{"x64", u16opt(7), ArchX64, true},
		{"arm64", u16opt(9), ArchARM64, true},
		{"first recognized wins", append(u16opt(0), u16opt(9)...), ArchBIOS, true},
		{"unknown code", u16opt(99), "", false},
		{"empty", nil, "", false},
		{"odd length", []byte{0, 0, 0}, ArchBIOS, true},
	}
	for _, c := range cases {
		a, ok := archFromBytes(c.v)
		if a != c.want || ok != c.ok {
			t.Errorf("%s: arch=%q ok=%v, want %q %v", c.name, a, ok, c.want, c.ok)
		}
	}
	// Vendor class fallback carries the code as five hex digits.
	for class, want := range map[string]Arch{
		"PXEClient:Arch:00000:UNDI:002001": ArchBIOS,
		"PXEClient:Arch:00007:UNDI:003016": ArchX64,
		"PXEClient:Arch:00009:UNDI":        ArchARM64,
	} {
		a, ok := archFromVendorClass(class)
		if !ok || a != want {
			t.Errorf("vendor class %s: got %q %v, want %q", class, a, ok, want)
		}
	}
	if a, ok := archFromVendorClass("PXEClient:Arch:0FFFF:UNDI"); ok {
		t.Errorf("unknown vendor-class arch accepted: %q", a)
	}
}

func TestIsIPXEVariants(t *testing.T) {
	var mac [6]byte
	// Option 175 alone marks iPXE even without the user class.
	b := discover(1, mac, option{optFeatures, []byte{1, 1}})
	if p, _ := parse(b); !p.isIPXE() {
		t.Error("option 175 not recognized as iPXE")
	}
	// RFC 3004 user-class is length-prefixed and may carry several classes.
	b = discover(1, mac, option{optUserClass, []byte{4, 'i', 'P', 'X', 'E', 3, 'f', 'o', 'o'}})
	if p, _ := parse(b); !p.isIPXE() {
		t.Error("user-class iPXE not recognized")
	}
	b = discover(1, mac, option{optUserClass, []byte{3, 'g', 'r', 'u'}})
	if p, _ := parse(b); p.isIPXE() {
		t.Error("non-iPXE user-class flagged")
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"52:54:00:12:34:56": "52:54:00:12:34:56",
		"52-54-00-12-34-56": "52:54:00:12:34:56",
		"5254:0012:3456":    "52:54:00:12:34:56",
		"525400123456":      "52:54:00:12:34:56",
		"AA:BB:CC:DD:EE:FF": "aa:bb:cc:dd:ee:ff",
		"":                  "",
		"nonsense":          "nonsense",
	}
	for in, want := range cases {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReplyGolden pins the wire format of the two reply flavors: a plain
// PXE ROM gets NBP name + TFTP server; iPXE gets the HTTP script URL.
// Firmware interop is unforgiving about these bytes, so they are pinned.
func TestReplyGolden(t *testing.T) {
	s := testServer(t, Options{
		NextServer: []byte{192, 168, 77, 1},
		BaseURL:    "http://192.168.77.1:8080",
	})
	var mac [6]byte
	copy(mac[:], []byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56})

	t.Run("plain pxe rom", func(t *testing.T) {
		req := discover(0x11223344, mac,
			option{optVendorClass, []byte("PXEClient:Arch:00000:UNDI:002001")},
			option{optArch, u16opt(0)},
		)
		reply := s.handle(req, 67)
		if reply == nil {
			t.Fatal("want a reply")
		}
		p, err := parse(reply)
		if err != nil {
			t.Fatalf("reply does not parse: %v", err)
		}
		if p.op != opReply || p.xid != 0x11223344 || p.mac() != "52:54:00:12:34:56" {
			t.Fatalf("identity fields wrong: op=%d xid=%x mac=%s", p.op, p.xid, p.mac())
		}
		if p.messageType() != msgOffer {
			t.Fatalf("want OFFER, got %d", p.messageType())
		}
		// BOOTP file field carries the NBP name for ROMs that read it.
		if got := hexClean(p.file[:]); got != "undionly.kpxe" {
			t.Errorf("file field = %q", got)
		}
		if sid, ok := p.options[optServerID]; !ok || string(sid) != "\xc0\xa8\x4d\x01" {
			t.Errorf("server id = %x", sid)
		}
		if bf, ok := p.options[optBootfile]; !ok || string(bf) != "undionly.kpxe" {
			t.Errorf("bootfile = %q", bf)
		}
		if ts, ok := p.options[optTFTPServer]; !ok || string(ts) != "192.168.77.1" {
			t.Errorf("tftp server = %q", ts)
		}
		if string(p.yiaddr[:]) != "\x00\x00\x00\x00" {
			t.Error("proxyDHCP must never assign an address")
		}
	})

	t.Run("ipxe client gets script url", func(t *testing.T) {
		req := discover(0x55667788, mac,
			option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003019")},
			option{optUserClass, []byte{4, 'i', 'P', 'X', 'E'}},
		)
		reply := s.handle(req, 67)
		p, _ := parse(reply)
		if bf := string(p.options[optBootfile]); bf != "http://192.168.77.1:8080/netboot/script?mac=52:54:00:12:34:56&arch=uefi-x64" {
			t.Errorf("bootfile = %q", bf)
		}
		if _, ok := p.options[optTFTPServer]; ok {
			t.Error("iPXE path must not carry a TFTP server option")
		}
	})
}

func TestHandleSilence(t *testing.T) {
	s := testServer(t, Options{
		NextServer: []byte{10, 0, 0, 1},
		BaseURL:    "http://10.0.0.1:8080",
	})
	var mac [6]byte
	copy(mac[:], []byte{0x52, 0x54, 0x00, 0xaa, 0xbb, 0xcc})

	req := discover(1, mac, option{optVendorClass, []byte("PXEClient:Arch:00000:UNDI")})
	req[0] = opRequest
	// Flip message type to REQUEST selecting the site DHCP — must stay silent.
	req2 := discover(1, mac, option{optVendorClass, []byte("PXEClient:Arch:00000:UNDI")},
		option{optMessageType, []byte{msgRequest}},
		option{optServerID, []byte{10, 0, 0, 254}})
	if s.handle(req2, 67) != nil {
		t.Error("REQUEST for another server id must not be answered on :67")
	}
	if s.handle(req2, 4011) == nil {
		t.Error("REQUEST on :4011 is boot-server discovery — must be answered")
	}
	// Non-PXE client: silence.
	req3 := discover(1, mac)
	if s.handle(req3, 67) != nil {
		t.Error("plain DHCP client must not be answered")
	}
	// Unrecognized arch: silence (firmware falls through).
	req4 := discover(1, mac, option{optVendorClass, []byte("PXEClient:Arch:00099:UNDI")},
		option{optArch, u16opt(99)})
	if s.handle(req4, 67) != nil {
		t.Error("unknown architecture must not be answered")
	}
	_ = req
}

// testServer builds a Server with the NBP filesystem present so reply
// paths pass the hasNBP gate.
func testServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.DHCPPort == 0 {
		opts.DHCPPort = 67
	}
	if opts.ProxyPort == 0 {
		opts.ProxyPort = 4011
	}
	s := &Server{
		opts:  opts,
		log:   discardLogger(),
		nbpOK: map[string]bool{"undionly.kpxe": true, "ipxe-amd64.efi": true, "ipxe-arm64.efi": true},
		done:  make(chan error, 1),
	}
	return s
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func hexClean(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return hex.EncodeToString(b)
}
