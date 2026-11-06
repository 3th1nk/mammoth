package bmcweb

import (
	"net/http"

	"github.com/3th1nk/mammoth/internal/store"
	"net/url"
	"strings"
	"testing"
)

const testToken = "devtoken"

func testProxy() *Proxy { return New(nil, nil, nil, testToken) }

func TestNormalizeTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string // scheme://host ("" = rejected)
	}{
		{"192.168.1.74", "https://192.168.1.74:443"},
		{"192.168.1.74:443", "https://192.168.1.74:443"},
		{"http://10.0.0.9:8080", "http://10.0.0.9:8080"},
		{"https://bmc.example.com", "https://bmc.example.com:443"},
		{"", ""},                    // empty
		{"fake://agentlog", ""},     // no web surface
		{"  ", ""},                  // blank
		{"ftp://x.example.com", ""}, // non-web scheme
	}
	for _, c := range cases {
		u := normalizeTarget(c.in)
		got := ""
		if u != nil {
			got = u.Scheme + "://" + u.Host
		}
		if got != c.want {
			t.Errorf("normalizeTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGateTicketRoundtrip(t *testing.T) {
	p := testProxy()
	ticket := p.sign("mch_1", 9999999999)
	if !strings.HasPrefix(ticket, "mch_1.9999999999.") {
		t.Fatalf("ticket shape wrong: %q", ticket)
	}
	if got := p.MachineFromGate(&http.Request{Header: http.Header{"Cookie": []string{GateCookie + "=" + ticket}}}); got != "mch_1" {
		t.Fatalf("MachineFromGate = %q, want mch_1", got)
	}
	// tamper: signature / expiry / machine
	bad := "mch_2.9999999999." + strings.SplitN(ticket, ".", 3)[2]
	if p.MachineFromGate(&http.Request{Header: http.Header{"Cookie": []string{GateCookie + "=" + bad}}}) != "" {
		t.Error("tampered machine accepted")
	}
	if p.MachineFromGate(&http.Request{Header: http.Header{"Cookie": []string{GateCookie + "=mch_1.1." + strings.SplitN(ticket, ".", 3)[2]}}}) != "" {
		t.Error("expired ticket accepted")
	}
	// cross-token keys must not verify
	other := New(nil, nil, nil, "othertoken")
	if other.MachineFromGate(&http.Request{Header: http.Header{"Cookie": []string{GateCookie + "=" + ticket}}}) != "" {
		t.Error("ticket from another token accepted")
	}
	// no cookie at all
	if p.MachineFromGate(&http.Request{}) != "" {
		t.Error("empty cookie accepted")
	}
}

func TestStripSecureCookie(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "SessionId=abc; Path=/; Secure; Httponly")
	resp.Header.Add("Set-Cookie", "tiny=1; Path=/")
	stripSecureCookie(resp)
	got := resp.Header.Values("Set-Cookie")
	if len(got) != 2 {
		t.Fatalf("cookie count = %d, want 2", len(got))
	}
	if strings.Contains(strings.ToLower(got[0]), "secure") {
		t.Errorf("Secure attribute survived: %q", got[0])
	}
	if !strings.Contains(strings.ToLower(got[0]), "httponly") {
		t.Errorf("HttpOnly lost: %q", got[0])
	}
	if got[1] != "tiny=1; Path=/" {
		t.Errorf("plain cookie mangled: %q", got[1])
	}
}

func TestLauncherShellAndResolution(t *testing.T) {
	p := testProxy()
	p.Register(stubLauncher{})
	hw := &store.Machine{ID: "mch_1", Model: strptr("2288H V5"), Vendor: strptr("Huawei"), BMCAddress: "192.168.1.74", BMCProtocol: "redfish"}
	html := p.launcherHTML(hw, "root", "p@ss<w>")
	// neutral shell facts
	for _, want := range []string{"mch_1", "2288H V5", "192.168.1.74", "huawei", "const BOOT"} {
		if !strings.Contains(html, want) {
			t.Errorf("launcher shell missing %q", want)
		}
	}
	// vendor script ran through resolution (stub marker) and creds are
	// script-context-escaped by json.Marshal
	for _, want := range []string{"stub-boot-script", `p@ss\u003cw\u003e`} {
		if !strings.Contains(html, want) {
			t.Errorf("launcher missing %q", want)
		}
	}
	// unregistered vendor falls back to the generic proxy-only launcher
	other := &store.Machine{ID: "mch_2", Vendor: strptr("H3C"), BMCAddress: "10.0.0.9", BMCProtocol: "redfish"}
	html2 := p.launcherHTML(other, "u", "p")
	if !strings.Contains(html2, "generic") || !strings.Contains(html2, "location.href = '/'") {
		t.Errorf("generic fallback missing for H3C machine")
	}
	if strings.Contains(html2, "stub-boot-script") {
		t.Error("huawei launcher matched a non-huawei machine")
	}
}

type stubLauncher struct{}

func (stubLauncher) Name() string { return "huawei" }
func (stubLauncher) Matches(m *store.Machine) bool {
	return m.Vendor != nil && strings.Contains(strings.ToLower(*m.Vendor), "huawei")
}
func (stubLauncher) BootScript(*store.Machine) string {
	return "stub-boot-script;"
}

func strptr(s string) *string { return &s }

func TestTargetContextRoundtrip(t *testing.T) {
	u, _ := url.Parse("https://x.example.com:443")
	if u.Port() != "443" {
		t.Fatalf("port parse = %q", u.Port())
	}
}
