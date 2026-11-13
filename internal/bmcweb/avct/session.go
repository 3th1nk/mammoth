package avct

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Session is one Avocent console session: control socket (login, input,
// keepalive) plus the video socket when the appliance uses a separate one.
//
// Lifecycle: Connect (dial + login) → OpenVideo (when required) → the video
// goroutine drives frames → Close. Input methods are safe once RUNNING.
type Session struct {
	cfg Config

	ctrl  net.Conn
	video net.Conn

	wmu  sync.Mutex // serialises control-socket writes
	vwmu sync.Mutex // serialises video-socket writes

	mu           sync.Mutex
	lr           *LoginResponse
	state        State
	clientRandom int32
	videoAcked   int
	stream       *VideoStream
	done         chan struct{}
	closeOnce    sync.Once
}

// SetVideoStream binds the video decoder. Frames arriving on either socket
// are fed to it — the appliance's login flags decide which socket carries
// video, and the Video Connect Status (0x84) may surface on either (the
// Java viewer runs a reader on both; docs/compat/dell.md §视频通道).
func (s *Session) SetVideoStream(vs *VideoStream) { s.stream = vs }

// State is the coarse session phase.
type State int

const (
	StateConnecting State = iota
	StateAuthenticating
	StateVideoPending
	StateRunning
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateConnecting:
		return "CONNECTING"
	case StateAuthenticating:
		return "AUTHENTICATING"
	case StateVideoPending:
		return "VIDEO_PENDING"
	case StateRunning:
		return "RUNNING"
	default:
		return "CLOSED"
	}
}

// Config describes one console session.
type Config struct {
	Host        string
	ControlPort int // kmport (iDRAC: 5900)
	VideoPort   int // vport  (iDRAC: 5900)
	Username    string
	Password    string
	Shared      bool // answer "in use" prompts by requesting a shared session
	TLSConfig   *tls.Config
	Timeout     time.Duration // dial/handshake/login bounds; 0 = 30s
	KeepAlive   time.Duration // 0 = 2s (the viewer's heartbeat cadence)
	Logger      Logger
	TraceVideo  bool // hex-dump raw video-socket traffic
	Trace       bool // hex-dump control-socket login/auth frames
}

// Logger is the minimal logging face the session needs.
type Logger interface {
	Printf(format string, args ...any)
}

// LogFunc adapts a plain function.
type LogFunc func(format string, args ...any)

func (f LogFunc) Printf(format string, args ...any) { f(format, args...) }

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// NewSession prepares a session; nothing is dialed yet.
func NewSession(cfg Config) *Session {
	if cfg.ControlPort == 0 {
		cfg.ControlPort = 5900
	}
	if cfg.VideoPort == 0 {
		cfg.VideoPort = cfg.ControlPort
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = nopLogger{}
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	return &Session{
		cfg:          cfg,
		state:        StateConnecting,
		clientRandom: int32(binary2(rnd) % 10_000_000),
		done:         make(chan struct{}),
	}
}

func binary2(b [4]byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// ClientRandom is the session id echoed in the video auth.
func (s *Session) ClientRandom() int32 { return s.clientRandom }

// LoginResponse is set once Connect succeeds.
func (s *Session) LoginResponse() *LoginResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lr
}

func (s *Session) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	s.cfg.Logger.Printf("avct: state -> %s", st)
}

// State returns the current phase.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Connect dials the control socket, performs APCP + TLS, sends the login
// request and waits for the login response.
func (s *Session) Connect(ctx context.Context) error {
	conn, res, err := DialAPCP(ctx, s.cfg.Host, s.cfg.ControlPort, APCPTypeControl, s.cfg.Timeout, s.cfg.TLSConfig)
	if err != nil {
		s.setState(StateClosed)
		return err
	}
	s.mu.Lock()
	s.ctrl = conn
	s.mu.Unlock()
	s.cfg.Logger.Printf("avct: control socket up (tls=%v, server protocol %d.%d)", res.TLS, res.Version[0], res.Version[1])

	// Login port byte rule (the viewer's): 1 with pre-2.0 appliances, the
	// low byte of KMPORT otherwise.
	portByte := byte(1)
	if res.Version[0] >= 2 {
		portByte = byte(s.cfg.ControlPort & 0xFF)
	}
	var share uint8
	if s.cfg.Shared {
		share = shareShared
	}
	login := controlFrame(TypeLoginRequest, loginRequestBody(s.cfg.Username, s.cfg.Password, portByte, 0, s.clientRandom, share))
	if s.cfg.Trace {
		s.cfg.Logger.Printf("avct: login frame clientRandom=%d body[200:]=% x", s.clientRandom, login.Body[200:209])
	}
	if err := s.sendControl(login); err != nil {
		s.Close()
		return fmt.Errorf("avct: send login: %w", err)
	}
	s.setState(StateAuthenticating)

	deadline := time.Now().Add(s.cfg.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		_ = conn.SetReadDeadline(deadline)
		f, err := ReadFrame(conn, false)
		if err != nil {
			s.Close()
			return fmt.Errorf("avct: waiting for login response: %w", err)
		}
		if f == nil {
			continue // in-band skip
		}
		if f.Type != TypeLoginResponse {
			s.cfg.Logger.Printf("avct: pre-login frame 0x%04x (%d bytes) ignored", f.Type, len(f.Body))
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		lr, err := decodeLoginResponse(f.Body)
		if err != nil {
			s.Close()
			return err
		}
		s.mu.Lock()
		s.lr = lr
		s.mu.Unlock()
		if !lr.Accepted() {
			s.Close()
			return &LoginError{Status: lr.Status, Reason: loginReason(lr.Status), Message: lr.Message}
		}
		s.cfg.Logger.Printf("avct: login accepted (applianceRandom=%d flags=0x%04x msg=%q)",
			lr.ApplianceRandom, lr.Flags, lr.Message)
		if lr.VideoOnControl() {
			s.setState(StateRunning)
		} else {
			s.setState(StateVideoPending)
		}
		return nil
	}
}

// OpenVideo opens the video socket when the login response says video does
// not ride the control channel, performs the channel auth, and starts the
// frame pump feeding fb. Idempotent for control-channel video.
func (s *Session) OpenVideo(ctx context.Context, fb *Framebuffer) error {
	lr := s.LoginResponse()
	if lr == nil || !lr.Accepted() {
		return errors.New("avct: OpenVideo before a successful login")
	}
	if lr.VideoOnControl() {
		return nil
	}
	conn, res, err := DialAPCP(ctx, s.cfg.Host, s.cfg.VideoPort, APCPTypeVideo, s.cfg.Timeout, s.cfg.TLSConfig)
	if err != nil {
		return err
	}
	s.cfg.Logger.Printf("avct: video socket up (tls=%v, server protocol %d.%d)", res.TLS, res.Version[0], res.Version[1])
	if s.cfg.TraceVideo {
		conn = &traceRW{Conn: conn, log: s.cfg.Logger}
	}
	s.mu.Lock()
	s.video = conn
	s.mu.Unlock()

	auth := videoFrame(TypeVideoChannelAuth, videoAuthBody(s.clientRandom, lr.ApplianceRandom))
	s.cfg.Logger.Printf("avct: video auth frame: %x", auth.videoBytes())
	s.vwmu.Lock()
	_, werr := conn.Write(auth.videoBytes())
	s.vwmu.Unlock()
	if werr != nil {
		s.Close()
		return fmt.Errorf("avct: video auth: %w", werr)
	}

	go s.videoLoop(conn)
	return nil
}

// traceRW hex-dumps video-socket traffic around the raw reads/writes.
type traceRW struct {
	net.Conn
	log Logger
}

func (t *traceRW) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		t.log.Printf("avct: video <<< %d bytes: % x", n, b[:min2(n, 96)])
	}
	return n, err
}

func (t *traceRW) Write(b []byte) (int, error) {
	t.log.Printf("avct: video >>> %d bytes: % x", len(b), b[:min2(len(b), 96)])
	return t.Conn.Write(b)
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// videoLoop pumps the video socket into the decoder. The Video Enable /
// display-geometry requests go out when the Connect Status arrives — which
// the control-channel loop and this loop both watch for.
func (s *Session) videoLoop(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		if s.video == conn {
			s.video = nil
		}
		s.mu.Unlock()
		conn.Close()
	}()
	for {
		f, err := ReadFrame(conn, true)
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			s.cfg.Logger.Printf("avct: video socket: %v", err)
			s.Close()
			return
		}
		if f == nil {
			continue
		}
		s.dispatchFrame(conn, f)
		select {
		case <-s.done:
			return
		default:
		}
	}
}

// dispatchFrame routes one frame to the session logic + video decoder.
func (s *Session) dispatchFrame(source net.Conn, f *Frame) {
	if f.Type&0xFF == 0x84 { // Video Connect Status: 0 = connected
		if len(f.Body) > 0 && f.Body[0] == 0 {
			s.videoConnected(source)
		}
		return
	}
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return
	}
	stream.Feed(f, func(ack bool) { s.ackVideo(source, ack) })
}

// videoConnected sends the enable/geometry triple and marks RUNNING.
func (s *Session) videoConnected(source net.Conn) {
	if s.State() == StateRunning {
		return
	}
	s.cfg.Logger.Printf("avct: video connected")
	for _, pkt := range []struct {
		t uint16
		b []byte
	}{
		{TypeVideoEnable, videoEnableBody(true)},
		{TypeSetDisplayArea, displayAreaBody(1024, 768)},
		{TypeSetScaleMode1to1, nil},
	} {
		// sendControl serialises writes itself; no outer lock here.
		if werr := s.sendControl(controlFrame(pkt.t, pkt.b)); werr != nil {
			s.cfg.Logger.Printf("avct: video enable: %v", werr)
		}
	}
	s.setState(StateRunning)
}

// ackVideo sends the video flow-control packet on the video socket; the
// device stops sending after 20 unacked packets.
func (s *Session) ackVideo(conn net.Conn, ackNeeded bool) {
	if !ackNeeded {
		return
	}
	s.mu.Lock()
	s.videoAcked++
	n := s.videoAcked
	s.videoAcked = 0
	s.mu.Unlock()
	ack := controlFrame(TypeVideoAck, videoAckBody(uint8(n)))
	s.vwmu.Lock()
	_, err := conn.Write(ack.Bytes())
	s.vwmu.Unlock()
	if err != nil {
		s.cfg.Logger.Printf("avct: video ack: %v", err)
	}
}

// sendControl writes one frame on the control socket.
func (s *Session) sendControl(f *Frame) error {
	s.mu.Lock()
	conn := s.ctrl
	s.mu.Unlock()
	if conn == nil {
		return errors.New("avct: control socket closed")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write(f.Bytes())
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// KeyDown sends a USB HID key press.
func (s *Session) KeyDown(usage uint16) error {
	return s.sendControl(controlFrame(TypeKeyboardData, keyboardBody(usage, true)))
}

// KeyUp sends a USB HID key release.
func (s *Session) KeyUp(usage uint16) error {
	return s.sendControl(controlFrame(TypeKeyboardData, keyboardBody(usage, false)))
}

// Pointer sends an absolute mouse event (video-pixel coordinates; buttons
// bit0 left / bit1 right / bit2 middle; wheel positive up).
func (s *Session) Pointer(buttons uint8, x, y, wheel int) error {
	return s.sendControl(controlFrame(TypeMouseData, mouseBody(buttons, x, y, wheel)))
}

// Refresh asks the appliance for a full-frame redraw (after noVNC connects).
func (s *Session) Refresh() error {
	return s.sendControl(controlFrame(TypeScreenRefresh, nil))
}

// Run starts the keepalive heartbeat and the control-channel reader, then
// blocks until Close or ctx cancellation. The reader is mandatory: the
// appliance streams video on the control socket when the login flags say so
// and answers video-connect status there regardless.
func (s *Session) Run(ctx context.Context) {
	go s.keepAliveLoop(ctx)
	go s.controlLoop()
}

func (s *Session) keepAliveLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.KeepAlive)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-s.done:
			return
		case <-t.C:
			if err := s.sendControl(controlFrame(TypeKeepAlive, nil)); err != nil {
				s.cfg.Logger.Printf("avct: keepalive: %v", err)
				s.Close()
				return
			}
		}
	}
}

// controlLoop reads every frame the appliance sends on the control socket.
func (s *Session) controlLoop() {
	s.mu.Lock()
	conn := s.ctrl
	s.mu.Unlock()
	if conn == nil {
		return
	}
	for {
		f, err := ReadFrame(conn, false)
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			s.cfg.Logger.Printf("avct: control socket: %v", err)
			s.Close()
			return
		}
		if f == nil {
			continue // in-band skip
		}
		// Everything (video-on-control frames, 0x84 connect status,
		// resolution updates) routes through the same dispatcher.
		s.dispatchFrame(conn, f)
	}
}

// Wait blocks until the session closes.
func (s *Session) Wait() { <-s.done }

// Close shuts both sockets and the keepalive loop. Safe to call repeatedly.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		c, v := s.ctrl, s.video
		s.ctrl, s.video = nil, nil
		s.state = StateClosed
		s.mu.Unlock()
		if v != nil {
			_ = v.Close()
		}
		if c != nil {
			_ = c.Close()
		}
	})
}
