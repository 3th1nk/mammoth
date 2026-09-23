package netboot

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
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

// captureHandler collects records for assertions on what the service logged.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler            { return c }

func (c *captureHandler) find(msg string) (attrs map[string]any, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Message != msg {
			continue
		}
		attrs = map[string]any{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
		return attrs, true
	}
	return nil, false
}

// TestSyslogSink drives the installer-log sink end to end: a lease-backed
// sender's line lands with task_id (the TaskLogTee contract), a foreign
// sender's line still lands with the source IP only.
func TestSyslogSink(t *testing.T) {
	// The pool hands the loopback address the test sends from — macFor then
	// resolves the sender, mirroring the pool-lease shape on a real L2. The
	// lease is granted mid-test (macOS lo0 carries only 127.0.0.1, so both
	// senders share the address): first the foreign shape, then the leased.
	pool, err := NewDHCPPool([]byte{127, 0, 0, 1}, []byte{127, 0, 0, 1},
		[]byte{255, 0, 0, 0}, []byte{127, 0, 0, 1})
	if err != nil {
		t.Fatal(err)
	}
	const mac = "52:54:00:ab:cd:ef"
	cap := &captureHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Start(ctx, Options{
		DHCPPort:   freeUDPPort(t),
		ProxyPort:  freeUDPPort(t),
		TFTPPort:   freeUDPPort(t),
		SyslogPort: freeUDPPort(t),
		NextServer: []byte{127, 0, 0, 1},
		BaseURL:    "http://127.0.0.1:8080",
		NBPs:       fstest.MapFS{},
		DHCP:       pool,
		Resolver: ResolverFunc(func(_ context.Context, m string) (*Entry, error) {
			if m == mac {
				return &Entry{MAC: m, TaskID: "task-slog"}, nil
			}
			return nil, nil
		}),
		Log: slog.New(cap),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sink := s.opts.SyslogPort

	send := func(src net.IP, payload string) {
		t.Helper()
		conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: src}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sink})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}

	// Foreign sender (no lease yet): logged, source only.
	send(net.IPv4(127, 0, 0, 1), "bare line without pri")
	waitFor(t, func() bool {
		attrs, ok := cap.find("bare line without pri")
		return ok && attrs["syslog_src"] == "127.0.0.1" && attrs["task_id"] == nil
	})

	// Leased sender: PRI stripped, task_id resolved (task_logs tee contract —
	// the field name must stay obs.FieldTaskID).
	if ip := pool.Reserve(mac); ip == nil {
		t.Fatal("reserve failed")
	}
	send(net.IPv4(127, 0, 0, 1), "<34>Mar  1 12:00:00 debian-installer: partman failed")
	waitFor(t, func() bool {
		attrs, ok := cap.find("Mar  1 12:00:00 debian-installer: partman failed")
		return ok && attrs["task_id"] == "task-slog" && attrs["syslog_src"] == "127.0.0.1"
	})

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

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

// parseSyslog accepts the forms the installers actually emit and caps rogue
// oversized lines — a diagnostic sink, never a parser contract.
func TestParseSyslog(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<34>Mar  1 12:00:00 debian-installer: partman failed", "Mar  1 12:00:00 debian-installer: partman failed"},
		{"<13>1 2026-09-17T12:00:00.0Z host app - - - five-twelve-four", "1 2026-09-17T12:00:00.0Z host app - - - five-twelve-four"},
		{"bare line without pri", "bare line without pri"},
		{"<34>   \t  ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseSyslog([]byte(c.in)); got != c.want {
			t.Errorf("parseSyslog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := "<34>" + strings.Repeat("x", 2000)
	if got := parseSyslog([]byte(long)); len(got) != 1024 {
		t.Errorf("oversized line not capped: %d", len(got))
	}
}

// vMedia attribution: no DHCP pool exists on that carrier — the sink falls
// back to the provision-registered address attributions, and unregistered
// senders stay source-only.
func TestSyslogIPRegistryAttribution(t *testing.T) {
	cap := &captureHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ips := NewIPRegistry()
	ips.Register("task-vmedia", []string{"127.0.0.1"}, time.Minute)
	s, err := Start(ctx, Options{
		DHCPPort:   freeUDPPort(t),
		ProxyPort:  freeUDPPort(t),
		TFTPPort:   freeUDPPort(t),
		SyslogPort: freeUDPPort(t),
		NextServer: []byte{127, 0, 0, 1},
		BaseURL:    "http://127.0.0.1:8080",
		NBPs:       fstest.MapFS{},
		IPResolver: ips,
		Log:        slog.New(cap),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.opts.SyslogPort})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("<13>vmedia installer line")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitFor(t, func() bool {
		attrs, ok := cap.find("vmedia installer line")
		return ok && attrs["task_id"] == "task-vmedia"
	})

	// Forget (the release paths) drops the attribution: the next line from
	// the same address is source-only again.
	ips.Forget("task-vmedia")
	conn, err = net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.opts.SyslogPort})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("post-release line")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitFor(t, func() bool {
		attrs, ok := cap.find("post-release line")
		return ok && attrs["task_id"] == nil && attrs["syslog_src"] == "127.0.0.1"
	})

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

// Pure virtual-media deployments (PXE off) start the service in its
// sink-only shape: no NextServer needed, only 514 binds, syslog attribution
// through the registry works, shutdown is clean.
func TestSyslogOnlyStart(t *testing.T) {
	cap := &captureHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ips := NewIPRegistry()
	ips.Register("task-sink", []string{"127.0.0.1"}, time.Minute)
	s, err := Start(ctx, Options{
		SyslogOnly: true,
		SyslogPort: freeUDPPort(t),
		BaseURL:    "http://127.0.0.1:8080",
		IPResolver: ips,
		Log:        slog.New(cap),
	})
	if err != nil {
		t.Fatalf("syslog-only start: %v", err)
	}

	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.opts.SyslogPort})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("sink-only line")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitFor(t, func() bool {
		attrs, ok := cap.find("sink-only line")
		return ok && attrs["task_id"] == "task-sink"
	})

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
