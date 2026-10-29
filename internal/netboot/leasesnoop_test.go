package netboot

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

var snoopMAC = [6]byte{0x52, 0x54, 0x00, 0xaa, 0x01, 0x24}

func snoopMACString() string { return "52:54:00:aa:01:24" }

func TestLeaseSnoopBasics(t *testing.T) {
	s := NewLeaseSnoop()
	ip := net.IPv4(192, 168, 1, 124)

	if got := s.macFor(ip); got != "" {
		t.Fatalf("unknown ip resolved %q", got)
	}
	s.Observe(snoopMACString(), ip)
	if got := s.macFor(ip); got != snoopMACString() {
		t.Fatalf("macFor = %q, want %q", got, snoopMACString())
	}
	// Newest observation wins.
	s.Observe("52:54:00:aa:01:99", ip)
	if got := s.macFor(ip); got != "52:54:00:aa:01:99" {
		t.Fatalf("re-observe: macFor = %q", got)
	}
	// Junk observations are ignored, not stored.
	s.Observe("", net.IPv4(192, 168, 1, 200))
	s.Observe(snoopMACString(), net.IPv4zero)
	s.Observe(snoopMACString(), net.ParseIP("2001:db8::1"))
	if got := s.macFor(net.IPv4(192, 168, 1, 200)); got != "" {
		t.Fatalf("empty mac stored: %q", got)
	}
}

func TestLeaseSnoopTTLAndEviction(t *testing.T) {
	s := newLeaseSnoop(10*time.Millisecond, 2)
	ip := net.IPv4(10, 0, 0, 1)
	s.Observe("52:54:00:00:00:01", ip)
	if got := s.macFor(ip); got == "" {
		t.Fatal("fresh binding must resolve")
	}
	time.Sleep(15 * time.Millisecond)
	if got := s.macFor(ip); got != "" {
		t.Fatalf("expired binding resolved %q", got)
	}

	// Overflow: the stalest live binding yields to the new one.
	s2 := newLeaseSnoop(time.Minute, 2)
	s2.Observe("52:54:00:00:00:01", net.IPv4(10, 0, 0, 1))
	time.Sleep(2 * time.Millisecond)
	s2.Observe("52:54:00:00:00:02", net.IPv4(10, 0, 0, 2))
	s2.Observe("52:54:00:00:00:03", net.IPv4(10, 0, 0, 3))
	if got := s2.macFor(net.IPv4(10, 0, 0, 1)); got != "" {
		t.Fatalf("stalest binding must be evicted, got %q", got)
	}
	if got := s2.macFor(net.IPv4(10, 0, 0, 3)); got != "52:54:00:00:00:03" {
		t.Fatalf("newest binding lost: %q", got)
	}
}

func TestBoundIP(t *testing.T) {
	siteIP := []byte{192, 168, 1, 124}
	mk := func(opts ...option) *packet {
		b := discover(1, snoopMAC, append(opts,
			option{optMessageType, []byte{msgRequest}})...)
		p, err := parse(b)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return p
	}
	nonZeroSrc := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 124), Port: 4011}

	// Option 50 wins over ciaddr and source.
	p := mk(option{optRequestedIP, siteIP})
	if got := boundIP(p, nonZeroSrc); !got.Equal(net.IPv4(192, 168, 1, 124)) {
		t.Fatalf("option50: got %v", got)
	}
	// ciaddr next.
	p2b := &packet{options: map[byte][]byte{}}
	copy(p2b.ciaddr[:], siteIP)
	if got := boundIP(p2b, nil); got == nil || !got.Equal(net.IPv4(192, 168, 1, 124)) {
		t.Fatalf("ciaddr: got %v", got)
	}
	// Source last; an address-less broadcast boot ROM learns nothing.
	p3 := mk()
	if got := boundIP(p3, &net.UDPAddr{IP: net.IPv4zero, Port: 68}); got != nil {
		t.Fatalf("address-less: got %v", got)
	}
	if got := boundIP(p3, nonZeroSrc); got == nil || !got.Equal(net.IPv4(192, 168, 1, 124)) {
		t.Fatalf("source: got %v", got)
	}
	// A zero option-50 payload must not shadow ciaddr.
	p4b := &packet{options: map[byte][]byte{optRequestedIP: {0, 0, 0, 0}}}
	copy(p4b.ciaddr[:], siteIP)
	if got := boundIP(p4b, nil); got == nil || !got.Equal(net.IPv4(192, 168, 1, 124)) {
		t.Fatalf("zero option50 must fall through to ciaddr, got %v", got)
	}
}

// TestHandleLearnsSnoopBinding pins the placement of the learning hook: a
// REQUEST that selected the site DHCP is not ours to answer, but it is ours
// to learn from — and the address-less DISCOVER must poison nothing.
func TestHandleLearnsSnoopBinding(t *testing.T) {
	snoop := NewLeaseSnoop()
	s := testServer(t, Options{
		NextServer: []byte{192, 168, 1, 107},
		BaseURL:    "http://192.168.1.107:8080",
		Snoop:      snoop,
	})
	clientIP := net.IPv4(192, 168, 1, 124)
	src := &net.UDPAddr{IP: clientIP, Port: 4011}

	// Address-less DISCOVER: nothing to learn yet.
	req := discover(1, snoopMAC,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	)
	if _, _ = s.handle(req, 67, &net.UDPAddr{IP: net.IPv4zero, Port: 68}); snoop.macFor(clientIP) != "" {
		t.Fatal("DISCOVER without an address must not learn")
	}

	// REQUEST selecting the site DHCP (option 50 = the leased address):
	// silence on :67, binding learned anyway.
	reqSel := discover(2, snoopMAC,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optMessageType, []byte{msgRequest}},
		option{optRequestedIP, []byte{192, 168, 1, 124}},
		option{optServerID, []byte{192, 168, 1, 1}},
	)
	if r, _ := s.handle(reqSel, 67, src); r != nil {
		t.Fatal("site-DHCP REQUEST must not be answered on :67")
	}
	if got := snoop.macFor(clientIP); got != snoopMACString() {
		t.Fatalf("binding not learned from silenced REQUEST: %q", got)
	}

	// The bootstrap REQUEST on :4011 (unicast from the bound client).
	reqBoot := discover(3, snoopMAC,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optMessageType, []byte{msgRequest}},
	)
	if r, _ := s.handle(reqBoot, 4011, src); r == nil {
		t.Fatal("bootstrap REQUEST on :4011 must be answered")
	}
}

// TestGrubConfigViaSnoop pins the proxyDHCP resolution chain end to end:
// the fixed /grub/grub.cfg path renders the armed entry's config through a
// snoop-learned binding, an unknown client gets the clean exit fallback,
// and foreign file names stay static.
func TestGrubConfigViaSnoop(t *testing.T) {
	snoop := NewLeaseSnoop()
	snoop.Observe(snoopMACString(), net.IPv4(192, 168, 1, 124))
	opts := Options{
		BaseURL: "http://192.168.1.107:8080",
		Resolver: ResolverFunc(func(_ context.Context, mac string) (*Entry, error) {
			if mac != snoopMACString() {
				return nil, nil
			}
			return &Entry{
				MAC: mac, TaskID: "tsk_x", Kind: "install", Token: "tok123",
				Kernel: "vmlinuz", Initrd: "initrd.img", KernelArgs: "inst.ks=http://x/ks.cfg",
			}, nil
		}),
		Snoop: snoop,
	}
	ctx := context.Background()

	cfg := grubConfigFor(opts, ctx, "grub/grub.cfg", net.IPv4(192, 168, 1, 124))
	if cfg == nil {
		t.Fatal("snoop-learned client must get a config")
	}
	out := string(cfg)
	for _, want := range []string{"(http,192.168.1.107:8080)/netboot/files/tok123/vmlinuz", "inst.ks=http://x/ks.cfg"} {
		if !strings.Contains(out, want) {
			t.Errorf("config missing %q:\n%s", want, out)
		}
	}

	// A client with no binding and no entry: the per-MAC name renders the
	// clean-exit fallback, the fixed path resolves to nothing.
	if cfg := grubConfigFor(opts, ctx, "grub/grub.cfg-01-52:54:00:00:00:99", net.IPv4(192, 168, 1, 99)); cfg == nil || !strings.Contains(string(cfg), "no pending boot entry") {
		t.Errorf("unknown per-MAC name must render the exit fallback, got:\n%s", cfg)
	}
	if cfg := grubConfigFor(opts, ctx, "grub/grub.cfg", net.IPv4(192, 168, 1, 99)); cfg != nil {
		t.Errorf("unknown fixed-path client must stay nil (static miss), got:\n%s", cfg)
	}
	// Foreign file names: static, never rendered.
	if cfg := grubConfigFor(opts, ctx, "shimx64.efi", net.IPv4(192, 168, 1, 124)); cfg != nil {
		t.Errorf("static NBP names must not be rendered, got:\n%s", cfg)
	}
}
