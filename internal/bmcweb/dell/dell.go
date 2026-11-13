// Dell launcher: Dell iDRAC machines skip the vendor web UI entirely — the
// engine runs the Avocent console protocol server-side (internal/bmcweb/avct)
// and the browser gets noVNC (docs/compat/dell.md §KVM 集成方案). The boot
// script is a single same-origin navigation into the engine-owned console
// panel; the iDRAC web session, viewer.jnlp ticket and port-5900 session all
// happen inside the websocket handler.
package dell

import (
	"strings"

	"github.com/3th1nk/mammoth/internal/bmcweb/avct"
	"github.com/3th1nk/mammoth/internal/store"
)

// Driver implements bmcweb.Launcher for Dell PowerEdge BMCs (iDRAC 6/7/8).
type Driver struct{}

// New constructs the launcher.
func New() *Driver { return &Driver{} }

// Name is the registry name.
func (*Driver) Name() string { return "dell" }

// Matches picks Dell machines. discovery stores the Redfish manufacturer
// verbatim and lower-cased ("dell inc."), so a substring match covers the
// whole family (docs/compat/dell.md §实测环境).
func (*Driver) Matches(m *store.Machine) bool {
	if m == nil || m.BMCProtocol == "none" || m.BMCAddress == "" || m.Vendor == nil {
		return false
	}
	return strings.Contains(strings.ToLower(*m.Vendor), "dell")
}

// BootScript navigates to the engine-owned noVNC panel. Everything else
// (login, ticket, 5900) happens inside the websocket handler; no BMC-origin
// page load is involved, which also sidesteps the broken static web root of
// real iDRAC7 firmware (docs/compat/dell.md §静态资源).
func (*Driver) BootScript(m *store.Machine) string {
	_ = avct.DefaultTLSConfig // referenced to keep the avct package linked here
	return `
    log('Dell iDRAC 虚拟控制台由引擎桥接 (noVNC)。');
    location.href = '/bmcweb/' + BOOT.machine + '/kvm';`
}
