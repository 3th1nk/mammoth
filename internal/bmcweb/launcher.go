package bmcweb

import (
	"encoding/json"
	"fmt"
	"html"

	"github.com/3th1nk/mammoth/internal/store"
)

// Launcher is the vendor extension point of the console proxy: one
// implementation per BMC family that knows how to boot its web console /
// KVM from inside the proxied origin. The engine's routes, gate cookie and
// proxy stay vendor-neutral (统一入口); a driver only contributes the boot
// script the launcher page runs. Register implementations on Proxy; the
// machine's Vendor field picks the driver, with the generic fallback below
// serving unregistered vendors as a plain proxy (manual login).
//
// Contract: the launcher page evaluates the BootScript body after defining
// `BOOT` (machine/address/vendor/user/password JSON), `log(msg[, cls])` and
// `fail(msg)` helpers. The script runs as the body of an async IIFE and may
// be async/await — it talks only to same-origin proxied paths. Credentials
// reach the script through BOOT's JSON (html-escaped by json.Marshal);
// drivers never template secrets into their script text.
type Launcher interface {
	// Name is the registry name ("huawei", later "h3c", "dell", ...).
	Name() string
	// Matches picks the driver for a machine (case-insensitive vendor
	// matching is the usual rule; discovery fills machine.Vendor from the
	// Redfish manufacturer string).
	Matches(m *store.Machine) bool
	// BootScript returns the vendor launch sequence (script body).
	BootScript(m *store.Machine) string
}

// genericFallback is the launcher for machines with no registered driver:
// the proxy still fronts the BMC web UI (operator logs in through it), but
// there is no automated launch sequence. Never registered — the default.
type genericFallback struct{}

func (genericFallback) Name() string                { return "generic" }
func (genericFallback) Matches(*store.Machine) bool { return true }

func (genericFallback) BootScript(*store.Machine) string {
	return `
    log('该机型尚未注册厂商启动序列 —— 进入代理模式。');
    log('在打开的 BMC 页面中手动登录,控制台入口与厂商 Web 一致。');
    location.href = '/';`
}

// launcherHTML is the console-session landing page (engine origin, gate
// cookie already set): the vendor-neutral shell that owns the page chrome,
// the boot data and the helper API, then runs the resolved driver's boot
// script. Credentials ride this page's markup once (the operator could read
// them from the credentials API anyway — same trust domain).
func (p *Proxy) launcherHTML(m *store.Machine, user, pass string) string {
	l := p.launcherFor(m)
	vendor := l.Name()
	if m.Vendor != nil && *m.Vendor != "" {
		vendor = *m.Vendor + " · " + vendor
	}
	boot := map[string]string{
		"machine":  m.ID,
		"name":     displayName(m),
		"address":  m.BMCAddress,
		"vendor":   vendor,
		"user":     user,
		"password": pass,
	}
	// json.Marshal HTML-escapes by default (< > & → \u00XX): the credential
	// cannot close or inject the surrounding <script> element.
	bootJSON, _ := json.Marshal(boot)
	name := displayName(m)
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-cn">
<head>
<meta charset="utf-8">
<title> mammoth console · %s </title>
<style>
  body { font: 14px/1.6 system-ui, sans-serif; background: #14161a; color: #d7dce3;
         display: grid; place-items: center; min-height: 90vh; margin: 0; }
  .card { max-width: 460px; padding: 28px 32px; background: #1d2026; border-radius: 10px; }
  h1 { font-size: 16px; margin: 0 0 12px; }
  code { background: #2a2e36; padding: 1px 5px; border-radius: 4px; }
  .log { margin-top: 14px; font-size: 12px; color: #8a94a3; white-space: pre-wrap; }
  .ok { color: #5fd08a; } .err { color: #ff7a7a; }
  button { margin-top: 14px; padding: 8px 18px; border: 0; border-radius: 6px;
           background: #3b82f6; color: #fff; font-size: 14px; cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
</style>
</head>
<body>
<div class="card">
  <h1>虚拟控制台 · %s <span style="color:#8a94a3;font-weight:normal">(%s)</span></h1>
  <p>BMC <code>%s</code> · 登录由本浏览器经引擎代理完成,凭证不落盘。</p>
  <div class="log" id="log">启动中…</div>
  <button id="retry" onclick="location.reload()" disabled>重试</button>
</div>
<script>
const BOOT = %s;
const log = (m, cls) => { const el = document.getElementById('log');
  el.innerHTML += (el.innerHTML ? '\n' : '') + m;
  if (cls) el.className = cls; };
const fail = m => { log(m, 'err'); document.getElementById('retry').disabled = false; };
(async () => {
  try {
%s
  } catch (e) {
    fail('异常: ' + e);
  }
})();
</script>
</body>
</html>`, html.EscapeString(name), html.EscapeString(name),
		html.EscapeString(vendor), html.EscapeString(m.BMCAddress),
		string(bootJSON), l.BootScript(m))
}

func displayName(m *store.Machine) string {
	if m.Model != nil && *m.Model != "" {
		return *m.Model
	}
	return m.ID
}
