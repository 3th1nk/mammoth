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

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/bmcweb/avct"
	"github.com/3th1nk/mammoth/internal/bmcweb/dell"
	"github.com/3th1nk/mammoth/internal/bmcweb/h3c"
	"github.com/3th1nk/mammoth/internal/store"
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
	case strings.HasSuffix(rest, "/kvm/power"):
		id, tail = strings.TrimSuffix(rest, "/kvm/power"), "power"
	case strings.HasSuffix(rest, "/kvm/media"):
		id, tail = strings.TrimSuffix(rest, "/kvm/media"), "media"
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
	case "power":
		p.kvmPower(c, id)
	case "media":
		p.kvmMedia(c, id)
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

	// H3C HDM: a plain RFB/VNC server on 5900 — the engine proxies it with
	// the VNC authentication carried on the credential password (truncated
	// to the RFB 8-byte key), no decoding at all (docs/compat/h3c.md §KVM).
	if isH3C(m) {
		netConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		bridgeLog := avct.LogFunc(func(format string, args ...any) {
			obsLog.Printf("rfb[%s]: "+format, append([]any{machineID}, args...)...)
		})
		if err := h3c.BridgeRFB(netConn, strings.TrimSuffix(m.BMCAddress, "/")+":5900", truncateRFBPassword(pass), bridgeLog); err != nil && ctx.Err() == nil {
			obsLog.Printf("kvm[%s]: rfb bridge: %v", machineID, err)
		}
		closeWithReason(websocket.StatusNormalClosure, "")
		return
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

// isH3C mirrors the h3c launcher's vendor match: the branch decides which
// console transport the device side gets (RFB proxy vs avct session).
func isH3C(m *store.Machine) bool {
	return m.Vendor != nil && strings.Contains(strings.ToLower(*m.Vendor), "h3c")
}

// truncateRFBPassword clamps the credential password to the RFB 8-byte key
// (the HDM settings page enforces the same limit).
func truncateRFBPassword(pass string) string {
	if len(pass) > 8 {
		return pass[:8]
	}
	return pass
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

// kvmPower executes a chassis power action through the driver registry
// (gate cookie trust domain, same as the console session). Body:
// {"action": "power_on"|"power_off"|"soft_off"|"hard_reboot"|"soft_reboot"|"cycle"}.
func (p *Proxy) kvmPower(c *gin.Context, machineID string) {
	if got := p.MachineFromGate(c.Request); got == "" || got != machineID {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "SCHEMA_UNAUTHORIZED", "status": 401,
			"detail": "console session missing or for another machine"})
		return
	}
	if p.BMC == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "SCHEMA_AUTH_UNCONFIGURED", "status": 503,
			"detail": "driver registry not wired"})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "SCHEMA_BAD_BODY", "status": 400, "detail": err.Error()})
		return
	}
	action := bmc.PowerAction(body.Action)
	switch action {
	case bmc.PowerOn, bmc.PowerOff, bmc.SoftOff, bmc.HardReboot, bmc.SoftReboot, bmc.Cycle:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"code": "SCHEMA_BAD_BODY", "status": 400,
			"detail": "unknown power action " + body.Action})
		return
	}
	m, user, pass, ok := p.resolve(c, machineID)
	if !ok {
		return
	}
	if got := p.MachineFromGate(c.Request); got == "" || got != machineID {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "SCHEMA_UNAUTHORIZED", "status": 401,
			"detail": "console session belongs to another machine"})
		return
	}
	_, err := p.BMC.Do(c.Request.Context(), m.BMCAddress, bmc.Credentials{Username: user, Password: pass},
		bmc.Protocol(m.BMCProtocol), "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
			return nil, d.SetPower(ctx, m.BMCAddress, bmc.Credentials{Username: user, Password: pass}, action)
		})
	if err != nil {
		obsLog.Printf("kvm[%s]: power %s: %v", machineID, action, err)
		c.JSON(http.StatusBadGateway, gin.H{"code": "BMC_POWER_FAILED", "status": 502, "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"action": action, "machine": machineID})
}

// kvmMedia mounts (POST {"url": "<iso url>"}) or ejects (DELETE) the
// virtual-media CD through the driver's MountMedia/EjectMedia.
func (p *Proxy) kvmMedia(c *gin.Context, machineID string) {
	if got := p.MachineFromGate(c.Request); got == "" || got != machineID {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "SCHEMA_UNAUTHORIZED", "status": 401,
			"detail": "console session missing or for another machine"})
		return
	}
	if p.BMC == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "SCHEMA_AUTH_UNCONFIGURED", "status": 503,
			"detail": "driver registry not wired"})
		return
	}
	var imgURL string
	if c.Request.Method == http.MethodPost {
		var body struct {
			URL string `json:"url"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.URL) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": "SCHEMA_BAD_BODY", "status": 400,
				"detail": "media mount needs a JSON body with a url field"})
			return
		}
		imgURL = body.URL
	}
	m, user, pass, ok := p.resolve(c, machineID)
	if !ok {
		return
	}
	cred := bmc.Credentials{Username: user, Password: pass}
	_, err := p.BMC.Do(c.Request.Context(), m.BMCAddress, cred,
		bmc.Protocol(m.BMCProtocol), "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
			if c.Request.Method == http.MethodDelete {
				return nil, d.EjectMedia(ctx, m.BMCAddress, cred, bmc.MediaImage{URL: imgURL})
			}
			return nil, d.MountMedia(ctx, m.BMCAddress, cred, bmc.MediaImage{URL: imgURL})
		})
	if err != nil {
		obsLog.Printf("kvm[%s]: media %s %s: %v", machineID, c.Request.Method, imgURL, err)
		c.JSON(http.StatusBadGateway, gin.H{"code": "BMC_MEDIA_FAILED", "status": 502, "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"mounted": c.Request.Method == http.MethodPost, "url": imgURL, "machine": machineID})
}

// kvmHTML is the noVNC shell with the operator menu: power actions and
// virtual media through the engine (gate-cookie trust domain), hotkeys
// (held modifiers / combos / custom) and view scaling through the noVNC
// API. Functions that need AMI's proprietary IVTP channel (video
// recording, mouse-sync modes) are not on the RFB path — noted in the menu.
func kvmHTML(machineID string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-cn">
<head>
<meta charset="utf-8">
<title> mammoth console · %s </title>
<style>
  body { font: 14px/1.6 system-ui, sans-serif; background: #14161a; color: #d7dce3;
         margin: 0; display: grid; grid-template-rows: auto auto 1fr; height: 100vh; }
  header { padding: 8px 14px; background: #1d2026; display: flex; gap: 16px; align-items: center; }
  header b { font-size: 14px; }
  header .log { font-size: 12px; color: #8a94a3; white-space: pre-wrap; }
  .ok { color: #5fd08a; } .err { color: #ff7a7a; }
  .menu { display: flex; gap: 2px; padding: 4px 10px; background: #20242b; flex-wrap: wrap; }
  .menu details { position: relative; }
  .menu summary { padding: 3px 10px; cursor: pointer; border-radius: 5px; list-style: none; user-select: none; }
  .menu summary:hover, .menu details[open] > summary { background: #2a2e36; }
  .menu summary::-webkit-details-marker { display: none; }
  .menu .items { position: absolute; z-index: 10; min-width: 170px; background: #1d2026;
                 border: 1px solid #2a2e36; border-radius: 6px; padding: 4px 0; max-height: 70vh; overflow: auto; }
  .menu .items button { display: block; width: 100%%; text-align: left; padding: 6px 14px;
                 background: none; border: 0; color: #d7dce3; cursor: pointer; font: inherit; }
  .menu .items button:hover { background: #2a2e36; }
  .menu .items button.held { color: #5fd08a; }

  dialog#custom-dialog { background: #1d2026; color: #d7dce3; border: 1px solid #2a2e36;
                 border-radius: 8px; padding: 16px 18px; width: 360px; }
  dialog#custom-dialog::backdrop { background: rgba(0,0,0,.55); }
  dialog#custom-dialog label { font-size: 13px; color: #8a94a3; display: block; margin-bottom: 8px; }
  dialog#custom-dialog input { width: 100%%; box-sizing: border-box; padding: 7px 10px;
                 background: #14161a; color: #d7dce3; border: 1px solid #2a2e36;
                 border-radius: 5px; font: 13px inherit; }
  dialog#custom-dialog .dlg-actions { display: flex; gap: 8px; margin-top: 12px; justify-content: flex-end; }
  dialog#custom-dialog .dlg-actions button { padding: 6px 16px; border-radius: 5px; border: 0;
                 cursor: pointer; font: inherit; background: #2a2e36; color: #d7dce3; }
  dialog#custom-dialog .dlg-actions button.primary { background: #3b82f6; color: #fff; }
  dialog#custom-dialog .hint { font-size: 11px; color: #8a94a3; margin: 10px 0 0; }
  dialog#custom-dialog #custom-captured { min-height: 44px; background: #14161a; border: 1px solid #2a2e36;
                 border-radius: 5px; padding: 8px 10px; display: flex; gap: 6px; align-items: center;
                 flex-wrap: wrap; margin-bottom: 12px; }
  dialog#custom-dialog #custom-captured .chip { padding: 3px 10px; background: #2a2e36;
                 border-radius: 5px; font-size: 13px; }
  dialog#custom-dialog #custom-captured .chip.main { border: 1px solid #3b82f6; color: #93c5fd; }
  dialog#custom-dialog #custom-captured .empty { color: #5b6270; font-size: 13px; }
  .menu .items button.held { color: #5fd08a; }
  .menu .items button.active { color: #5fd08a; }
  .menu .group { padding: 5px 14px 2px; font-size: 11px; color: #8a94a3; border-top: 1px solid #2a2e36; margin-top: 4px; }
  .menu .group:first-child { border-top: 0; margin-top: 0; }
  .menu .plain { padding: 3px 10px; cursor: pointer; border-radius: 5px; background: none;
                 border: 0; color: #d7dce3; font: inherit; }
  .menu .plain:hover { background: #2a2e36; }
  .menu input.custom { width: 170px; margin: 4px 14px; padding: 4px 8px; background: #14161a;
                 color: #d7dce3; border: 1px solid #2a2e36; border-radius: 5px; font: 12px inherit; }
  .menu input.custom + button { padding: 5px 10px; }
  #screen { overflow: auto; background: #000; }
  #screen canvas { transform-origin: top left; }
</style>
<script type="module">
import RFB from '/bmcweb/assets/novnc/core/rfb.js';
const id = '%s';
const base = '/bmcweb/' + id + '/kvm';
const logEl = document.getElementById('log');
const log = (m, cls) => { logEl.innerHTML = m; logEl.className = cls ?? ''; };
let rfb = null;
try {
  rfb = new RFB(document.getElementById('screen'), base + '/ws');
  rfb.scaleViewport = true;
  rfb.background = '#000';
  rfb.addEventListener('connect', () => log('控制台已连接', 'ok'));
  rfb.addEventListener('disconnect', (e) => {
    const d = e.detail && e.detail.reason ? e.detail.reason : '';
    log('连接断开 ' + d, 'err');
  });
  window.rfb = rfb;
} catch (e) {
  log('初始化失败: ' + e, 'err');
}

const api = async (path, opts) => {
  const r = await fetch(path, opts);
  if (!r.ok) {
    const t = await r.text();
    throw new Error(t.slice(0, 140));
  }
  return r.json();
};

// ── 菜单互斥与点外收起 ──
const menus = Array.from(document.querySelectorAll('.menu details'));
document.addEventListener('click', (e) => {
  for (const d of menus) {
    if (d.open && !d.contains(e.target)) d.open = false;
  }
});
menus.forEach((d) => d.addEventListener('toggle', () => {
  if (d.open) menus.forEach((o) => { if (o !== d) o.open = false; });
}));

// ── 电源 ──
const POWER_ACTIONS = [
  ['power_on', '开机'], ['soft_off', '正常关机'], ['power_off', '强制断电'],
  ['soft_reboot', '正常重启'], ['hard_reboot', '强制重启'], ['cycle', '硬循环'],
];
const powerList = document.getElementById('power-items');
for (const [action, label] of POWER_ACTIONS) {
  const b = document.createElement('button');
  b.textContent = label;
  b.onclick = async () => {
    if (!confirm('确认执行：' + label + '？')) return;
    b.disabled = true;
    try {
      await api(base + '/power', { method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action }) });
      log('电源动作已发送：' + label, 'ok');
    } catch (e) { log('电源动作失败：' + e.message, 'err'); }
    b.disabled = false;
  };
  powerList.appendChild(b);
}

// ── 键盘 ──
const X = { enter: 0xFF0D, tab: 0xFF09, escape: 0xFF1B, esc: 0xFF1B,
            backspace: 0xFF08, delete: 0xFFFF, del: 0xFFFF, space: 0x20,
            home: 0xFF50, end: 0xFF57, pageup: 0xFF55, pagedown: 0xFF56,
            left: 0xFF51, up: 0xFF52, right: 0xFF53, down: 0xFF54,
            printscreen: 0xFF61, menu: 0xFF67, contextmenu: 0xFF67,
            ctrl_l: 0xFFE3, ctrl_r: 0xFFE4, alt_l: 0xFFE9, alt_r: 0xFFEA,
            win_l: 0xFFEB, win_r: 0xFFEC, shift_l: 0xFFE1, shift_r: 0xFFE2 };
for (let i = 1; i <= 12; i++) X['f' + i] = 0xFFBE + i - 1;
const parseKeysym = (name) => {
  const n = name.toLowerCase().trim();
  if (X[n] !== undefined) return X[n];
  if (/^[a-z]$/.test(n)) return 0x61 + n.charCodeAt(0) - 97;
  if (/^[0-9]$/.test(n)) return 0x30 + n.charCodeAt(0) - 48;
  if (n.length === 1) { const c = n.charCodeAt(0); if (c >= 0x20 && c <= 0x7E) return c; }
  return null;
};
const MODS = { ctrl: 'ctrl_l', ctlr: 'ctrl_l', alt: 'alt_l', shift: 'shift_l', win: 'win_l', meta: 'win_l' };

// 按住类修饰键（再点释放）
const held = new Map(); // keysym → button
function toggleHold(sym, name, btn) {
  if (held.has(sym)) {
    rfb.sendKey(sym, name, false);
    held.delete(sym);
    btn.classList.remove('held');
    log('已释放 ' + name, 'ok');
  } else {
    rfb.sendKey(sym, name, true);
    held.set(sym, btn);
    btn.classList.add('held');
    log('按住 ' + name + '（再点释放）', 'ok');
  }
}
function sendCombo(parts) {
  // parts: 修饰键在前按住，主键 press/release，修饰键逆序释放
  const syms = parts.map((p) => {
    const sym = parseKeysym(p);
    if (sym === null) throw new Error('未知按键 ' + p);
    return { sym, name: p };
  });
  for (let i = 0; i < syms.length - 1; i++) rfb.sendKey(syms[i].sym, syms[i].name, true);
  const last = syms[syms.length - 1];
  rfb.sendKey(last.sym, last.name, true);
  rfb.sendKey(last.sym, last.name, false);
  for (let i = syms.length - 2; i >= 0; i--) rfb.sendKey(syms[i].sym, syms[i].name, false);
}

const keyList = document.getElementById('key-items');
const group = (label) => {
  const d = document.createElement('div');
  d.className = 'group';
  d.textContent = label;
  keyList.appendChild(d);
};
const item = (label, fn) => {
  const b = document.createElement('button');
  b.textContent = label;
  b.onclick = fn;
  keyList.appendChild(b);
  return b;
};

group('按住（再点释放）');
const HOLD_KEYS = [['右Ctrl', 'ctrl_r'], ['右Alt', 'alt_r'], ['右Windows', 'win_r'],
                   ['左Ctrl', 'ctrl_l'], ['左Alt', 'alt_l'], ['左Windows', 'win_l']];
for (const [label, key] of HOLD_KEYS) {
  const sym = X[key];
  item(label, function () { toggleHold(sym, key, this); });
}
group('按下并松开');
item('Ctrl+Alt+Del', () => sendCombo(['ctrl_l', 'alt_l', 'delete']));
item('左Windows键', () => sendCombo(['win_l']));
item('右Windows键', () => sendCombo(['win_r']));
item('上下文菜单键', () => sendCombo(['menu']));
item('Print Screen', () => sendCombo(['printscreen']));
group('自定义');
item('自定义组合键…', () => document.getElementById('custom-dialog').showModal());

// ── 视图（适应窗口 / 百分比）──
const VIEW_MODES = [
  { id: 'fit', label: '适应窗口', fit: true },
  { id: '75', label: '75%%', scale: 0.75 },
  { id: '100', label: '100%%', scale: 1.0 },
  { id: '125', label: '125%%', scale: 1.25 },
  { id: '150', label: '150%%', scale: 1.5 },
];
let activeView = 'fit';
const viewList = document.getElementById('view-items');
function canvas() { return document.querySelector('#screen canvas'); }
function applyView(v) {
  if (!rfb) return;
  if (v.fit) {
    rfb.scaleViewport = true;
    const cv = canvas();
    if (cv) cv.style.transform = '';
  } else {
    rfb.scaleViewport = false;
    const cv = canvas();
    if (cv) cv.style.transform = 'scale(' + v.scale + ')';
  }
  activeView = v.id;
  Array.from(viewList.children).forEach((b) => {
    b.classList.toggle('active', b.dataset.view === v.id);
    b.textContent = (b.dataset.view === v.id ? '✓ ' : '') + b.dataset.label;
  });
}
for (const v of VIEW_MODES) {
  const b = document.createElement('button');
  b.dataset.view = v.id;
  b.dataset.label = v.label;
  b.textContent = v.label;
  b.onclick = () => applyView(v);
  viewList.appendChild(b);
}
applyView(VIEW_MODES[0]);

// ── 截屏 ──
document.getElementById('screenshot').onclick = () => {
  const cv = canvas();
  if (!cv) return log('无画面可截取', 'err');
  const a = document.createElement('a');
  a.href = cv.toDataURL('image/png');
  a.download = 'console-' + new Date().toISOString().replace(/[:.]/g, '-') + '.png';
  a.click();
  log('截图已保存', 'ok');
};

// ── 自定义组合键弹窗：键盘捕获 ──
const dlg = document.getElementById('custom-dialog');
const capBox = document.getElementById('custom-captured');
const sendBtn = document.getElementById('custom-send');
const CODE_NAMES = {
  ControlLeft: 'ctrl_l', ControlRight: 'ctrl_r', AltLeft: 'alt_l', AltRight: 'alt_r',
  MetaLeft: 'win_l', MetaRight: 'win_r', ShiftLeft: 'shift_l', ShiftRight: 'shift_r',
  Enter: 'enter', Backspace: 'backspace', Tab: 'tab', Escape: 'esc',
  Delete: 'delete', Insert: 'insert', Home: 'home', End: 'end',
  PageUp: 'pageup', PageDown: 'pagedown', ArrowLeft: 'left', ArrowUp: 'up',
  ArrowRight: 'right', ArrowDown: 'down', Space: 'space',
  Period: '.', Comma: ',', Slash: '/', Semicolon: ';', Quote: "'",
  BracketLeft: '[', BracketRight: ']', Backslash: '\\', Minus: '-', Equal: '=',
  Backquote: '\u0060',
};
const MOD_CODES = new Set(['ControlLeft', 'ControlRight', 'AltLeft', 'AltRight', 'MetaLeft', 'MetaRight', 'ShiftLeft', 'ShiftRight']);
let captured = [];      // 捕获的按键名序列
let locked = false;     // 主键按下后序列锁定

function renderCaptured() {
  capBox.innerHTML = '';
  if (!captured.length) {
    capBox.innerHTML = '<span class="empty">等待按键…</span>';
    sendBtn.disabled = true;
    return;
  }
  captured.forEach((name, i) => {
    const chip = document.createElement('span');
    chip.className = 'chip' + (i === captured.length - 1 ? ' main' : '');
    chip.textContent = name;
    capBox.appendChild(chip);
  });
  sendBtn.disabled = false;
}
function clearCaptured() { captured = []; locked = false; renderCaptured(); }

dlg.addEventListener('keydown', (e) => {
  e.preventDefault(); // 捕获模式：吞掉一切按键
  if (e.key === 'Escape') { dlg.close(); return; }
  if (e.code === 'Backspace') { captured.pop(); locked = false; renderCaptured(); return; }
  if (locked) return; // 主键按下后序列锁定，点清除重新录
  const code = e.code;
  if (code === 'Escape' || code === 'Backspace') return;
  let name = CODE_NAMES[code];
  if (name === undefined) {
    if (e.key && e.key.length === 1) name = e.key.toLowerCase();
    else if (/^Key[A-Z]$/.test(code)) name = code.slice(3).toLowerCase();
    else if (/^Digit[0-9]$/.test(code)) name = code.slice(5);
    else if (/^Numpad[0-9]$/.test(code)) name = 'numpad' + code.slice(6);
    else if (/^F[0-9]{1,2}$/.test(code)) name = 'f' + code.slice(1).toLowerCase();
    else return; // 未识别的键（纯修饰状态变化等）
  }
  captured.push(name);
  if (!MOD_CODES.has(code)) locked = true; // 主键落下即锁定序列
  renderCaptured();
});

document.getElementById('custom-clear').onclick = clearCaptured;
sendBtn.onclick = () => {
  if (!captured.length) return;
  try {
    sendCombo(captured);
    log('已发送 ' + captured.join('+'), 'ok');
    dlg.close();
    clearCaptured();
  } catch (e) { log('发送失败：' + e.message, 'err'); }
};
document.getElementById('custom-cancel').onclick = () => { dlg.close(); clearCaptured(); };

// ── CD 镜像（虚拟媒体）──
document.getElementById('media-mount').onclick = async () => {
  const url = document.getElementById('media-url').value.trim();
  if (!url) return log('先填入介质 URL', 'err');
  const b = document.getElementById('media-mount');
  b.disabled = true;
  try {
    await api(base + '/media', { method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url }) });
    log('介质已挂载：' + url, 'ok');
  } catch (e) { log('挂载失败：' + e.message, 'err'); }
  b.disabled = false;
};
document.getElementById('media-eject').onclick = async () => {
  try {
    await api(base + '/media', { method: 'DELETE' });
    log('介质已弹出', 'ok');
  } catch (e) { log('弹出失败：' + e.message, 'err'); }
};
</script>
</head>
<body>
<header>
  <b>虚拟控制台 · %s</b>
  <span class="log" id="log">连接中…</span>
</header>
<nav class="menu">
  <details id="m-power">
    <summary>电源</summary>
    <div class="items" id="power-items"></div>
  </details>
  <details id="m-keys">
    <summary>热键</summary>
    <div class="items" id="key-items"></div>
  </details>
  <details id="m-view">
    <summary>视图</summary>
    <div class="items" id="view-items"></div>
  </details>
  <button class="plain" id="screenshot">截屏</button>
  <details>
    <summary>虚拟介质</summary>
    <div class="items" style="padding:8px">
      <input class="media-url" id="media-url" placeholder="ISO 介质 URL">
      <div style="display:flex;gap:6px;margin-top:6px">
        <button id="media-mount">挂载</button>
        <button id="media-eject">弹出</button>
      </div>
    </div>
  </details>
  <span style="color:#5b6270;font-size:12px;margin-left:auto">视频录制/鼠标同步为 AMI 私有协议特性，RFB 通路不可用</span>
</nav>
<div id="screen"></div>
<dialog id="custom-dialog">
  <label>自定义组合键</label>
  <div id="custom-captured"><span class="empty">等待按键…</span></div>
  <div class="dlg-actions">
    <button id="custom-clear">清除</button>
    <button id="custom-send" class="primary" disabled>发送</button>
    <button id="custom-cancel">关闭</button>
  </div>
  <p class="hint">直接按下要发送的按键，逐键捕获；<br>Backspace 回退，Esc 关闭，Ctrl+Alt+Del 请用预置按钮。</p>
</dialog>
</body>
</html>`, html.EscapeString(machineID), machineID, html.EscapeString(machineID))
}
