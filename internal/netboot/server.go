package netboot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"

	"github.com/3th1nk/mammoth/internal/obs"
)

// Options configures one netboot service instance.
type Options struct {
	// DHCPPort is the proxyDHCP listen port (67). ProxyDHCP coexists with
	// the site DHCP across the L2 — a different host — never on the same
	// host: both want :67, and the OS refuses. See docs/operations.md.
	DHCPPort int
	// ProxyPort is the PXE boot-server discovery port (4011).
	ProxyPort int
	// TFTPPort is the NBP transfer port (69).
	TFTPPort int
	// NextServer is mammoth's IPv4 address on the provisioning L2 — the
	// siaddr/TFTP-server address offered to PXE clients.
	NextServer net.IP
	// BaseURL is the machine-face base URL (no trailing slash); iPXE
	// clients are pointed at <BaseURL>/netboot/script.
	BaseURL string
	// NBPs is the filesystem of network boot programs (embedded assets).
	NBPs fs.FS
	// Resolver answers boot lookups by MAC for the TFTP grub.cfg rendering
	// (Secure Boot chain). nil disables dynamic rendering — the TFTP service
	// then serves only the static NBP binaries.
	Resolver Resolver
	// OnObserve, when set, is called for every PXE client whose architecture
	// the responder resolved (option 93 / vendor class): the hook the serve
	// wiring uses to persist firmware observations into the machine record
	// (docs/08-data-model.md machines). Called inline on the DHCP read path
	// — keep it fast and non-blocking. nil keeps the responder
	// observation-free.
	OnObserve func(mac string, arch Arch)
	// BroadcastAddr, when set, replaces the limited broadcast
	// (255.255.255.255) in boot replies with this directed broadcast
	// (the provisioning subnet's 192.168.x.255). macOS routing sends
	// 255.255.255.255 via the default interface — guests behind a local
	// vmnet bridge never see the offer (qemu verification finding); Linux
	// deployments need no override (nil).
	BroadcastAddr net.IP
	// DHCP, when set, turns the responder into a full DHCP server for PXE
	// clients (option 60) on DHCP-less provisioning L2s — a boot ROM needs
	// an IP lease before it will fetch anything. nil keeps the pure proxy
	// model (the site DHCP owns addresses). See DHCPPool.
	DHCP *DHCPPool
	// SyslogPort is the installer-log sink port (514): d-i forwards its
	// ramfs syslog here via the syslog= kernel argument, and lines that
	// resolve to an armed netboot entry are logged with task_id so the
	// TaskLogTee files them into task_logs. Zero means the default 514.
	// Binding failures degrade to a warning — the sink is a diagnostic,
	// never a lifeline (unlike DHCP/TFTP there is no boot stranded by its
	// absence).
	SyslogPort int
	// ExternalOnly is the escape-hatch mode for deployment shapes where
	// mammoth cannot be the PXE service (containerized without privileged
	// UDP, or an operations team that owns DHCP/TFTP): only HTTP is served
	// by mammoth, an external DHCP+TFTP (dnsmasq) delivers the static boot
	// chain, and client identity self-reports through the trampolines the
	// ExportExternalKit materializes (iPXE ${net0/mac} → /netboot/script,
	// grub ${net_default_mac} → /netboot/grub). No DHCP is seen, so option-93
	// firmware observation never fires; the script endpoint's enrollment
	// fallback still works (identity is client-supplied). ExportExternalKit
	// is the companion the deployment ships to the TFTP root.
	ExternalOnly bool
	// IPResolver attributes syslog senders by IP when the lease-reverse
	// chain misses: vMedia machines hold no lease, so provision registers
	// the spec's declared static addresses here at boot (TTL-bound; the
	// release paths forget them). See IPRegistry. nil keeps lease-only
	// attribution.
	IPResolver *IPRegistry
	// SyslogOnly starts ONLY the installer-log sink — no DHCP/TFTP/HTTP
	// ownership. Pure virtual-media deployments (PXE off) run the netboot
	// service in this shape so the syslog channel (inst.syslog=/syslog= on
	// every carrier now) still lands in task_logs. Serve wiring picks
	// exactly one of the full start and this.
	SyslogOnly bool
	// Log receives service diagnostics; nil defaults to slog.Default().
	Log *slog.Logger
}

// broadcastFor resolves the reply broadcast target: the configured directed
// broadcast when set, else the limited broadcast.
func (o Options) broadcastFor() net.IP {
	if o.BroadcastAddr != nil {
		return o.BroadcastAddr
	}
	return net.IPv4bcast
}

// Server is one running netboot service (proxyDHCP + TFTP).
type Server struct {
	opts    Options
	log     *slog.Logger
	nbpOK   map[string]bool
	done    chan error
	closers []ioCloser
}

type ioCloser interface{ Close() error }

// Start binds the UDP services and serves them until ctx ends. Binding is
// synchronous: an error here is a deployment fault (port taken, missing
// privilege) and aborts startup — unlike the NFS export there is no
// external escape hatch, a silent downgrade would strand machines at the
// PXE prompt.
func Start(ctx context.Context, opts Options) (*Server, error) {
	if !opts.ExternalOnly && !opts.SyslogOnly {
		if v4 := opts.NextServer.To4(); v4 == nil {
			return nil, fmt.Errorf("netboot: NextServer must be an IPv4 address")
		}
	}
	if opts.DHCPPort == 0 {
		opts.DHCPPort = 67
	}
	if opts.ProxyPort == 0 {
		opts.ProxyPort = 4011
	}
	if opts.TFTPPort == 0 {
		opts.TFTPPort = 69
	}
	if opts.SyslogPort == 0 {
		opts.SyslogPort = 514
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		opts:  opts,
		log:   log,
		nbpOK: map[string]bool{},
		done:  make(chan error, 1),
	}

	if opts.ExternalOnly || opts.SyslogOnly {
		// No UDP ownership beyond the sink. ExternalOnly is the escape
		// hatch (site owns DHCP/TFTP); SyslogOnly is the pure virtual-media
		// shape — both bind ONLY the installer-log sink, diagnostics with
		// the same degradation semantics as builtin mode.
		s.closers = []ioCloser{}
		syslogDone := make(chan struct{})
		if syslogConn, serr := net.ListenUDP("udp4", &net.UDPAddr{Port: opts.SyslogPort}); serr != nil {
			log.Warn("netboot: installer syslog sink unavailable (install logs stay lost with the ramfs)",
				"port", opts.SyslogPort, "err", serr.Error())
			close(syslogDone)
		} else {
			s.closers = append(s.closers, syslogConn)
			go s.serveSyslog(ctx, syslogConn, syslogDone)
		}
		go func() {
			<-ctx.Done()
			for _, c := range s.closers {
				c.Close()
			}
			// Mirror the builtin shutdown: ctx cancellation reports nil.
			if err := ctx.Err(); errors.Is(err, context.Canceled) {
				s.done <- nil
			} else {
				s.done <- ctx.Err()
			}
		}()
		mode := "external: DHCP/TFTP owned by the site; mammoth serves HTTP only"
		if opts.SyslogOnly {
			mode = "syslog-only: pure virtual-media deployment, no PXE ownership"
		}
		log.Info("netboot service started ("+mode+")",
			"base_url", opts.BaseURL,
			"syslog_port", opts.SyslogPort)
		return s, nil
	}

	dhcpConn, err := listenReuse(opts.DHCPPort)
	if err != nil {
		return nil, fmt.Errorf("netboot: bind dhcp :%d (privileged port; a site DHCP on this host also claims it): %w", opts.DHCPPort, err)
	}
	proxyConn, err := listenReuse(opts.ProxyPort)
	if err != nil {
		dhcpConn.Close()
		return nil, fmt.Errorf("netboot: bind pxe :%d: %w", opts.ProxyPort, err)
	}
	tftpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: opts.TFTPPort})
	if err != nil {
		dhcpConn.Close()
		proxyConn.Close()
		return nil, fmt.Errorf("netboot: bind tftp :%d: %w", opts.TFTPPort, err)
	}
	s.closers = []ioCloser{dhcpConn, proxyConn, tftpConn}
	// The installer-log sink: a busy port (a site rsyslogd, a missing
	// privilege on an exotic deployment) costs diagnostics, not boots —
	// degrade to a warning where DHCP/TFTP above abort startup.
	var syslogConn *net.UDPConn
	if syslogConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: opts.SyslogPort}); err != nil {
		log.Warn("netboot: installer syslog sink unavailable (install logs stay lost with the ramfs)",
			"port", opts.SyslogPort, "err", err.Error())
		syslogConn = nil
	} else {
		s.closers = append(s.closers, syslogConn)
	}

	udpLog := func(format string, args ...any) { s.logf(format, args...) }

	dhcpDone := make(chan struct{})
	proxyDone := make(chan struct{})
	tftpDone := make(chan struct{})
	syslogDone := make(chan struct{})
	go s.serveDHCP(ctx, dhcpConn, dhcpDone)
	go s.serveProxy(ctx, proxyConn, proxyDone)
	if syslogConn != nil {
		go s.serveSyslog(ctx, syslogConn, syslogDone)
	} else {
		close(syslogDone)
	}
	// Dynamic TFTP rendering: grubnet fetches its config over TFTP before its
	// network stack is fully up. Debian grubnet's prefix is (tftp)/grub/ and —
	// under proxyDHCP, where net_default_server stays empty — it falls straight
	// to the fixed path /grub/grub.cfg rather than trying grub.cfg-01-<mac>.
	// The client is therefore resolved by its lease IP (reverse of the DHCP
	// pool's MAC→IP assignment). Everything else stays static.
	render := func(name string, remoteIP net.IP) []byte {
		if opts.Resolver == nil {
			return nil
		}
		mac := ""
		if m, ok := grubConfigMAC(name); ok {
			mac = m
		} else if name == "grub/grub.cfg" {
			if opts.DHCP != nil {
				mac = opts.DHCP.macFor(remoteIP)
			}
		}
		if mac == "" {
			return nil
		}
		e, err := opts.Resolver.Entry(ctx, mac)
		if err != nil || e == nil {
			return []byte(NoEntryGRUB(mac))
		}
		return []byte(RenderGRUB(e, opts.BaseURL))
	}
	go func() {
		defer close(tftpDone)
		serveTFTP(ctx, tftpConn, opts.NBPs, render, udpLog)
	}()
	go func() {
		<-ctx.Done()
		for _, c := range s.closers {
			c.Close()
		}
	}()
	go func() {
		<-dhcpDone
		<-proxyDone
		<-tftpDone
		<-syslogDone
		// A ctx cancellation is the normal shutdown path — report nil so
		// callers can distinguish "stopped on purpose" from a fault.
		if err := ctx.Err(); errors.Is(err, context.Canceled) {
			s.done <- nil
		} else {
			s.done <- ctx.Err()
		}
	}()
	log.Info("netboot service started",
		"dhcp_port", opts.DHCPPort, "pxe_port", opts.ProxyPort,
		"tftp_port", opts.TFTPPort, "next_server", opts.NextServer.String(),
		"base_url", opts.BaseURL)
	return s, nil
}

// Wait reports how the server ended (nil on clean shutdown via ctx).
func (s *Server) Wait() error { return <-s.done }

// logf emits a service diagnostic at info level.
func (s *Server) logf(format string, args ...any) {
	s.log.Info("netboot: " + fmt.Sprintf(format, args...))
}

// serveDHCP drains the proxyDHCP socket: read, decide, unicast the reply.
func (s *Server) serveDHCP(ctx context.Context, conn *net.UDPConn, done chan<- struct{}) {
	defer close(done)
	s.serveUDP(ctx, conn, s.opts.DHCPPort)
}

// serveProxy drains the PXE boot-server discovery socket (4011).
func (s *Server) serveProxy(ctx context.Context, conn *net.UDPConn, done chan<- struct{}) {
	defer close(done)
	s.serveUDP(ctx, conn, s.opts.ProxyPort)
}

// serveSyslog drains the installer-log sink: d-i forwards its ramfs syslog
// here via the syslog= kernel argument (related-work §2 — the installer
// environment dies with the ramfs, and the update-grub post-mortem nearly
// ran out of evidence without these lines). A line whose sender resolves to
// an armed netboot entry (pool lease IP → MAC → entry) is logged with
// task_id, which the TaskLogTee files into task_logs — `jobs logs` then
// shows the installer's own view of the install. Unresolvable senders are
// still logged (source IP only): foreign DHCP clients and enrollment
// probes chatter here too.
func (s *Server) serveSyslog(ctx context.Context, conn *net.UDPConn, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 2048) // BSD syslog datagrams stay under 1KiB; headroom for 5424
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		msg := parseSyslog(buf[:n])
		if msg == "" {
			continue
		}
		args := []any{"syslog_src", addr.IP.String()}
		taskID := ""
		if s.opts.DHCP != nil && s.opts.Resolver != nil {
			if mac := s.opts.DHCP.macFor(addr.IP); mac != "" {
				if e, terr := s.opts.Resolver.Entry(ctx, mac); terr == nil && e != nil && e.TaskID != "" {
					taskID = e.TaskID
				}
			}
		}
		// Lease chain missed (vMedia: no pool at all; site-DHCP proxy: the
		// lease is the site's) — fall back to the provision-registered
		// address attributions before giving up.
		if taskID == "" && s.opts.IPResolver != nil {
			taskID = s.opts.IPResolver.TaskForIP(addr.IP)
		}
		if taskID != "" {
			args = append(args, obs.FieldTaskID, taskID)
		}
		s.log.Info(msg, args...)
	}
}

// parseSyslog extracts the message body from a syslog datagram: the BSD
// form ("<PRI>Mmm dd hh:mm:ss host tag: msg") the installers emit, tolerant
// of RFC 5424 and bare lines — the sink is a diagnostic, never a parser
// contract. Empty when nothing usable remains; oversized lines are capped.
func parseSyslog(b []byte) string {
	msg := strings.TrimSpace(string(b))
	if strings.HasPrefix(msg, "<") {
		if end := strings.Index(msg, ">"); end > 0 {
			msg = strings.TrimSpace(msg[end+1:])
		}
	}
	const maxLine = 1024
	if len(msg) > maxLine {
		return msg[:maxLine]
	}
	return msg
}

func (s *Server) serveUDP(ctx context.Context, conn *net.UDPConn, port int) {
	// ipv4.PacketConn gives per-datagram interface knowledge: replies (in
	// particular the broadcast ones a boot ROM can only receive) leave
	// through the interface the request arrived on, not whatever the
	// routing table picks for 255.255.255.255.
	pc := ipv4.NewPacketConn(conn)
	_ = pc.SetControlMessage(ipv4.FlagInterface, true)
	buf := make([]byte, 1500)
	for {
		n, cm, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		reply, to := s.handle(buf[:n], port, addr.(*net.UDPAddr))
		if reply == nil {
			continue
		}
		// A datagram without control info (arrived before SetControlMessage
		// took effect, or sent without pktinfo) reads back cm == nil — the
		// reply then leaves via the routing table instead of the arrival
		// interface, which broadcast replies cannot afford to rely on.
		ifindex := 0
		if cm != nil {
			ifindex = cm.IfIndex
		}
		s.logf("dhcp: request from %s (ifindex %d) -> reply to %s", addr, ifindex, to)
		if _, err := pc.WriteTo(reply, &ipv4.ControlMessage{IfIndex: ifindex}, to); err != nil {
			s.logf("dhcp: reply to %s: %v", to, err)
		}
	}
}

// hasNBP reports whether the named boot program exists (cached — the set
// is embedded and immutable).
func (s *Server) hasNBP(name string) bool {
	ok, cached := s.nbpOK[name]
	if !cached {
		_, err := fs.Stat(s.opts.NBPs, name)
		ok = err == nil
		s.nbpOK[name] = ok
	}
	return ok
}

// listenReuse binds a UDP port with SO_REUSEADDR (rebind after restart
// without TIME_LATENCY style wait) and SO_BROADCAST (the boot ROM's DISCOVER
// arrives with the broadcast flag set, and RFC 2131 replies to it go to
// 255.255.255.255).
func listenReuse(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{}
	lc.Control = func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if serr == nil {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

// CachedResolver wraps any Resolver with a small TTL cache. It absorbs the
// PXE retry storm: firmware re-asks several times per boot and re-boots
// repeat the question; the store answer is stable on these timescales.
// Negative results are cached too, so machines without entries cost nothing
// — the TTL bounds how late a freshly registered entry is noticed (entries
// are registered well before boot).
type CachedResolver struct {
	inner Resolver
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]cacheRec
}

type cacheRec struct {
	entry *Entry
	err   error
	at    time.Time
}

const cacheLimit = 4096

// NewCachedResolver wraps inner with the given positive TTL.
func NewCachedResolver(inner Resolver, ttl time.Duration) *CachedResolver {
	return &CachedResolver{inner: inner, ttl: ttl, cache: map[string]cacheRec{}}
}

// Entry resolves through the cache.
func (c *CachedResolver) Entry(ctx context.Context, mac string) (*Entry, error) {
	c.mu.Lock()
	rec, ok := c.cache[mac]
	c.mu.Unlock()
	if ok && time.Since(rec.at) < c.ttl {
		return rec.entry, rec.err
	}
	e, err := c.inner.Entry(ctx, mac)
	c.mu.Lock()
	if len(c.cache) >= cacheLimit {
		c.cache = map[string]cacheRec{} // bound memory; a fresh map re-warms
	}
	c.cache[mac] = cacheRec{entry: e, err: err, at: time.Now()}
	c.mu.Unlock()
	return e, err
}
