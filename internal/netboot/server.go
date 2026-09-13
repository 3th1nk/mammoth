package netboot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
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
	// Log receives service diagnostics; nil defaults to slog.Default().
	Log *slog.Logger
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
	if v4 := opts.NextServer.To4(); v4 == nil {
		return nil, fmt.Errorf("netboot: NextServer must be an IPv4 address")
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

	udpLog := func(format string, args ...any) { s.logf(format, args...) }

	dhcpDone := make(chan struct{})
	proxyDone := make(chan struct{})
	tftpDone := make(chan struct{})
	go s.serveDHCP(ctx, dhcpConn, dhcpDone)
	go s.serveProxy(ctx, proxyConn, proxyDone)
	go func() {
		defer close(tftpDone)
		serveTFTP(ctx, tftpConn, opts.NBPs, udpLog)
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
		wcm := &ipv4.ControlMessage{}
		if cm != nil {
			wcm.IfIndex = cm.IfIndex
		}
		if _, err := pc.WriteTo(reply, wcm, to); err != nil {
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
