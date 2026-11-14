// H3C launcher: HDM (AMI MegaRAC base) ships an HTML5 KVM at
// /viewer.html — the AMI H5 Viewer — which authenticates off the HDM web
// session and speaks a same-origin websocket (wss://<hdm>/kvm, share mode;
// docs/compat/h3c.md §KVM). That makes it a proxy-origin citizen like the
// Huawei iBMC H5 client: the boot script logs the BROWSER into the HDM web
// API, seeds the session state the viewer reads (CSRF cookie + sessionStorage
// bootstraps), then navigates to /viewer.html — the whole vendor flow runs
// unmodified under the proxy origin, websocket upgrade included.
//
// Wire facts (real run 2026-11-14, UniServer R4900 G3 / HDM 3.16.00):
//   - POST /api/session/login, form "username=" then "password=" → JSON
//     {"cc":0, "CSRFToken":"..", "privilege":..} + Set-Cookie QSESSIONID
//     (cc != 0 is the failure code; the login endpoint counts failures).
//   - The viewer reads cookie "CSRF" and sessionStorage "garc" (the
//     CSRFToken), plus "privilege"/"features"/"status"/"extended_privilege"
//     bootstraps, and cookie "kvm_mode" (1 = shared, over the web port).
//   - Dedicated-port modes (9666/9667, plain ws) bypass the proxy origin —
//     kvm_mode stays on the shared form.
package h3c

import (
	"strings"

	"github.com/3th1nk/mammoth/internal/store"
)

// Driver implements bmcweb.Launcher for H3C HDM machines.
type Driver struct{}

// New constructs the launcher.
func New() *Driver { return &Driver{} }

// Name is the registry name.
func (*Driver) Name() string { return "h3c" }

// Matches picks H3C machines. discovery stores the Redfish manufacturer
// verbatim and lower-cased — HDM answers "new h3c technologies co., ltd."
// (real run 2026-11-14), so a substring match covers the family.
func (*Driver) Matches(m *store.Machine) bool {
	if m == nil || m.BMCProtocol == "none" || m.BMCAddress == "" || m.Vendor == nil {
		return false
	}
	return strings.Contains(strings.ToLower(*m.Vendor), "h3c")
}

// BootScript navigates straight to the AMI H5 Viewer with the credentials
// as hex parameters: the viewer performs its OWN login flow — it fetches
// /api/rsapublickey itself, RSA-encrypts the hex credentials, POSTs
// /api/session, seeds garc/kvm_access/vmedia_access and the X-CSRFTOKEN
// header (the whole sequence verified in the real capture). The KVM
// websocket then binds to the web port (WEB_APP_PROXY branch: ws on 80 /
// wss on 443, same origin), so the engine must listen on the standard web
// port for the proxy to carry it (docs/compat/h3c.md §KVM).
func (*Driver) BootScript(m *store.Machine) string {
	return `
    log('H3C HDM H5 KVM 由代理同源承载（viewer 自主登录）。');
    log('H5 KVM 由引擎桥接（noVNC over RFB 直通）。');
    location.href = '/bmcweb/' + BOOT.machine + '/kvm';`
}
