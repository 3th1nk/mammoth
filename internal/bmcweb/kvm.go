// Engine-owned console panel routes (bmcweb/{id}/kvm): the noVNC page and
// the RFB-over-websocket it connects to. Unlike the proxy catch-all, these
// routes are served by the engine itself — the gate cookie still names the
// machine (the session route planted it), and the engine speaks the Dell
// console protocol server-side:
//
//	browser noVNC ──websocket──▶ engine RFB bridge ──avct/TLS──▶ iDRAC :5900
//
// The bridge owns one iDRAC web session (login → viewer.jnlp ticket) per
// websocket and logs out on close, so iDRAC session slots never leak.

package bmcweb

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/bmcweb/avct"
	"github.com/3th1nk/mammoth/internal/bmcweb/dell"
)

const (
	// kvmConnectTimeout bounds each stage of bringing the console up (web
	// login, JNLP fetch, APCP/TLS dial, console login, video channel).
	kvmConnectTimeout = 45 * time.Second
)

// TryKVM intercepts the engine-owned KVM routes before the proxy catch-all:
//
//	GET /bmcweb/{id}/kvm      — the noVNC page (gate cookie required)
//	GET /bmcweb/{id}/kvm/ws   — the RFB websocket (gate cookie required)
//
// false keeps the caller's fallback when the path is not a KVM route.
func (p *Proxy) TryKVM(c *gin.Context) bool {
	path := c.Request.URL.Path
	if !strings.HasPrefix(path, "/bmcweb/") {
		return false
	}
	rest := strings.TrimPrefix(path, "/bmcweb/")
	var id, tail string
	switch {
	case strings.HasSuffix(rest, "/kvm/ws"):
		id, tail = strings.TrimSuffix(rest, "/kvm/ws"), "ws"
	case strings.HasSuffix(rest, "/kvm"):
		id, tail = strings.TrimSuffix(rest, "/kvm"), "page"
	default:
		return false
	}
	if id == "" || strings.Contains(id, "/") {
		return false
	}
	switch tail {
	case "page":
		p.kvmPage(c, id)
	case "ws":
		p.kvmWS(c, id)
	}
	return true
}

// kvmPage serves the noVNC shell. The gate cookie must already be set (the
// /bmcweb/{id}/session launcher page does it); same-origin navigation carries
// it, so no token has to appear in the page.
func (p *Proxy) kvmPage(c *gin.Context, machineID string) {
	// The gate cookie is bound to one machine: its id must equal the
	// requested one (opening another machine's panel rebinds the cookie via
	// the session route, never through a foreign gate).
	if got := p.MachineFromGate(c.Request); got == "" || got != machineID {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "SCHEMA_UNAUTHORIZED", "status": 401,
			"detail": "console session missing or for another machine"})
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(kvmHTML(machineID)))
}

// kvmWS upgrades to a websocket and bridges it to the machine's console:
// web login → viewer.jnlp ticket → APCP/TLS dial → avct login → video
// channel → RFB. Any failure before the console is RUNNING answers with a
// websocket close carrying the reason (the page shows it).
func (p *Proxy) kvmWS(c *gin.Context, machineID string) {
	m, user, pass, ok := p.resolve(c, machineID)
	if !ok {
		return
	}
	// Gate cookie must also match this machine (resolve does not check it).
	if got := p.MachineFromGate(c.Request); got == "" || got != machineID {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "SCHEMA_UNAUTHORIZED", "status": 401,
			"detail": "console session belongs to another machine"})
		return
	}

	ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		// noVNC offers subprotocols and may come from the same origin only;
		// answering none is fine for binary frames. Same-origin requests carry
		// the gate cookie, which already authenticated the session above.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	defer ws.Close(websocket.StatusInternalError, "bridge closed")

	ctx := c.Request.Context()
	dctx, cancel := context.WithTimeout(ctx, kvmConnectTimeout)
	defer cancel()

	closeWithReason := func(code websocket.StatusCode, msg string) {
		_ = ws.Close(code, msg)
	}

	// 1. iDRAC web session + console ticket.
	client, err := dell.NewClient(m.BMCAddress, user, pass, kvmConnectTimeout)
	if err != nil {
		closeWithReason(websocket.StatusInternalError, err.Error())
		return
	}
	if err := client.Login(dctx); err != nil {
		closeWithReason(websocket.StatusPolicyViolation, "iDRAC login: "+err.Error())
		return
	}
	// Best-effort logout on any exit path after a successful login.
	defer func() {
		lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer lcancel()
		_ = client.Logout(lctx)
	}()

	title := machineID
	if m.Model != nil && *m.Model != "" {
		title = *m.Model + ", " + machineID + ", User: " + user
	}
	cc, err := client.ConsoleLaunch(dctx, title)
	if err != nil {
		closeWithReason(websocket.StatusInternalError, "console ticket: "+err.Error())
		return
	}

	// 2. avct console session on the ticket credentials.
	sess := avct.NewSession(avct.Config{
		Host:        cc.Host,
		ControlPort: cc.KMPort,
		VideoPort:   cc.VPort,
		Username:    cc.User,
		Password:    cc.Password,
		Timeout:     kvmConnectTimeout,
		Logger: avct.LogFunc(func(format string, args ...any) {
			obsLog.Printf("avct[%s]: "+format, append([]any{machineID}, args...)...)
		}),
	})
	if err := sess.Connect(dctx); err != nil {
		closeWithReason(websocket.StatusInternalError, "console login: "+err.Error())
		return
	}
	sess.Run(ctx)

	fb := avct.NewFramebuffer(1024, 768)
	sess.SetVideoStream(avct.NewVideoStream(fb))
	if err := sess.OpenVideo(dctx, fb); err != nil {
		closeWithReason(websocket.StatusInternalError, "video channel: "+err.Error())
		return
	}

	// 3. RFB bridge over the websocket. NetConn serialises writes and turns
	// binary frames into a stream the RFB handshake can read.
	netConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	bridge := &avct.RFBBridge{
		FB:    fb,
		Input: sess,
		Name:  cc.Title,
		Logger: avct.LogFunc(func(format string, args ...any) {
			obsLog.Printf("rfb[%s]: "+format, append([]any{machineID}, args...)...)
		}),
	}
	if err := bridge.Serve(netConn); err != nil && !errors.Is(err, context.Canceled) {
		obsLog.Printf("kvm[%s]: bridge: %v", machineID, err)
	}
	sess.Close()
	closeWithReason(websocket.StatusNormalClosure, "")
}

// Logger is the minimal logging face the KVM bridge needs.
type Logger interface {
	Printf(format string, args ...any)
}

// obsLog is wired by the engine at startup (server.go) to the structured
// logger; the default keeps the package usable standalone.
var obsLog Logger = stdLogger{}

type stdLogger struct{}

func (stdLogger) Printf(format string, args ...any) {
	fmt.Printf("bmcweb: "+format+"\n", args...)
}

// SetLogger swaps the package logger.
func SetLogger(l Logger) {
	if l != nil {
		obsLog = l
	}
}

// kvmHTML is the noVNC shell (vendored assets under /bmcweb/assets/novnc/).
func kvmHTML(machineID string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-cn">
<head>
<meta charset="utf-8">
<title> mammoth console · %s </title>
<style>
  body { font: 14px/1.6 system-ui, sans-serif; background: #14161a; color: #d7dce3;
         margin: 0; display: grid; grid-template-rows: auto 1fr; height: 100vh; }
  header { padding: 8px 14px; background: #1d2026; display: flex; gap: 16px; align-items: center; }
  header b { font-size: 14px; }
  header .log { font-size: 12px; color: #8a94a3; }
  .ok { color: #5fd08a; } .err { color: #ff7a7a; }
  #screen { overflow: auto; background: #000; }
</style>
<script type="module">
import RFB from '/bmcweb/assets/novnc/core/rfb.js';
const logEl = document.getElementById('log');
const log = (m, cls) => { logEl.innerHTML = m; logEl.className = cls ?? ''; };
try {
  const rfb = new RFB(document.getElementById('screen'), '/bmcweb/%s/kvm/ws');
  rfb.scaleViewport = true;
  rfb.background = '#000';
  rfb.addEventListener('connect', () => log('控制台已连接', 'ok'));
  rfb.addEventListener('disconnect', (e) => {
    const d = e.detail && e.detail.reason ? e.detail.reason : '';
    log('连接断开 ' + d, 'err');
  });
  rfb.addEventListener('credentialsrequired', () => {
    // iDRAC 侧不向 RFB 客户端要求凭据（None 认证）；防御性提示。
    log('查看器要求凭据（不应发生）', 'err');
  });
  window.rfb = rfb;
} catch (e) {
  log('初始化失败: ' + e, 'err');
}
</script>
</head>
<body>
<header>
  <b>虚拟控制台 · %s</b>
  <span class="log" id="log">连接中…</span>
</header>
<div id="screen"></div>
</body>
</html>`, html.EscapeString(machineID), machineID, html.EscapeString(machineID))
}
