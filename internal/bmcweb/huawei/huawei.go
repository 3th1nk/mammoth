// Package huawei is the bmcweb launcher driver for Huawei iBMC web
// consoles (2288H V5 / iBMC 6.41 wired 2026-11-06; mechanism and evidence:
// docs/compat/huawei.md §KVM HTML5 集成远程控制台).
//
// The vendor launch flow keeps its state in the BMC origin's localStorage
// (a one-shot H5ClientModel flag consumed by kvm_h5.html) and the KVM page
// signs its ticket exchange with the login token stored in loginUerInfo —
// the boot script replays exactly the sequence the vendor home button runs,
// with the operator's browser as the session owner (engine-crafted BMC web
// sessions are dead on arrival — same doc, §会话模型).
package huawei

import (
	"strings"

	"github.com/3th1nk/mammoth/internal/store"
)

// Driver implements bmcweb.Launcher for Huawei iBMC.
type Driver struct{}

func New() Driver { return Driver{} }

func (Driver) Name() string { return "huawei" }

// Matches keys on the discovery vendor string (Redfish manufacturer —
// "Huawei", "Huawei Technologies Co., Ltd.", ...). Protocol "none" (no BMC)
// never matches: the caller rejects it before launcher resolution anyway.
func (Driver) Matches(m *store.Machine) bool {
	if m.BMCProtocol == "none" || m.BMCAddress == "" {
		return false
	}
	if m.Vendor == nil {
		return false
	}
	return strings.Contains(strings.ToLower(*m.Vendor), "huawei")
}

// BootScript is the launcher body (async context; the neutral shell provides
// BOOT — machine/address/user/password JSON — and log/fail helpers).
// Sequence per docs/compat/huawei.md §1: login → seed localStorage
// (loginUerInfo token / manufacturer / one-shot H5ClientModel) →
// RefreshActivationState → window.open kvm_h5.html. The KVM page then runs
// its own ticket exchange (ExportKvmStartupFile → GeneralDownload →
// kvm.jnlp) and the H5 client connects to wss://<IPA>:2198 directly from
// this browser.
func (Driver) BootScript(*store.Machine) string {
	return `
    // slot hygiene: closing a tab is NOT a logout — the previous console
    // run's web session lingers server-side until the BMC's timeout, and
    // repeated clicks pile up zombie slots against the small session cap
    // (~8; real run 2026-11-07: table full from a morning of test clicks).
    // The previous run's session id rides localStorage (shared across the
    // launcher's tabs) — retire it before minting a new one so repeated
    // clicks stay self-cleaning.
    const prevSid = localStorage.getItem('mammoth_sid');
    if (prevSid) {
      try {
        const dr = await fetch('/redfish/v1/SessionService/Sessions/' + prevSid, {
          method: 'DELETE', headers: { From: 'WebUI', 'X-Requested-With': 'XMLHttpRequest' },
        });
        log('清理上次会话 ' + prevSid.slice(0, 8) + '…: HTTP ' + dr.status);
      } catch (_) {}
    }
    log('登录 BMC (' + BOOT.vendor + ') …');
    const lr = await fetch('/redfish/v1/SessionService/Sessions/', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'From': 'WebUI' },
      body: JSON.stringify({ UserName: BOOT.user, Password: BOOT.password }),
    });
    if (lr.status !== 201) {
      let why = 'HTTP ' + lr.status;
      try {
        const eb = (await lr.json()).error['@Message.ExtendedInfo'][0];
        if (eb.MessageId === 'Base.1.0.SessionLimitExceeded') {
          why = 'BMC 会话数已达上限——关闭遗留的 KVM/控制台标签页(它们在轮询保活),' +
                '等待约 45 分钟会话自然过期后重试;急用可复位 BMC 管理子系统(短暂断 BMC 面)。';
        } else if (eb.Message) { why += ' — ' + eb.MessageId + ': ' + eb.Message; }
      } catch (_) {}
      fail('登录失败: ' + why);
      return;
    }
    // From:WebUI logins carry the credential in the 'Token' response header
    // (X-Auth-Token is only issued to Redfish-tagged logins).
    const token = lr.headers.get('Token') || lr.headers.get('X-Auth-Token') || '';
    const lbody = await lr.json();
    log('已登录 (session ' + (lbody.Id || '?') + ')', 'ok');

    // seed the vendor KVM page's reads: its ticket exchange signs with the
    // login token (loginUerInfo.token) and walks /Managers/<ManagerId> —
    // the iBMC manager id is always "1" (the session id is NOT the manager
    // id; seeding it there 404s the whole ticket path, 2026-11-06 E2E).
    // softwareName feeds the KVM page's document.title (null → "null <host>").
    localStorage.setItem('loginUerInfo', JSON.stringify({ token: token, ManagerId: '1' }));
    if (!localStorage.getItem('manufacturer')) localStorage.setItem('manufacturer', 'Huawei');
    if (!localStorage.getItem('softwareName')) localStorage.setItem('softwareName', 'iBMC');
    sessionStorage.setItem('sessionid', lbody.Id || '');
    localStorage.setItem('mammoth_sid', lbody.Id || ''); // next click retires this one

    // session activation — the vendor SPA does this on every page load and
    // before launching the console; the ticket path expects it.
    await fetch('/redfish/v1/SessionService/Sessions/' + (lbody.Id || '') +
      '/Oem/Huawei/Actions/Session.RefreshActivationState', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json;charset=UTF-8', 'From': 'WebUI' },
      body: JSON.stringify({ Mode: 'Activate' }),
    });

    // arm the vendor one-shot launch flag and BECOME the KVM page: a same
    // -origin navigation instead of window.open — the open ran after async
    // fetches, outside the click's gesture window, so browsers blocked it
    // as a popup (real run 2026-11-07); one tab total, nothing to allow.
    localStorage.setItem('H5ClientModel', 'Shared');
    localStorage.setItem('localH5KVMInfo', null);
    log('进入 KVM …(视频流由浏览器直连 BMC 的 2198 端口 wss)');
    location.href = '/src/virtualControl/kvm_h5.html?' + Math.random();`
}
