// Package bmcweb serves the operator-facing BMC web console through the
// engine: an authenticated reverse proxy that lets the operator's own
// browser drive the vendor web UI (KVM included) without reaching the BMC
// network or holding its credentials.
//
// Why a proxy and not a synthesized console URL (docs/compat/huawei.md §KVM
// 集成方案, 2026-11-06): the vendor launch flow keeps its state in the BMC
// origin's localStorage (a one-shot H5ClientModel flag), which no foreign
// origin can set — and engine-side-crafted BMC sessions are rejected by the
// BMC's client binding (dead-on-arrival sessions). The only working shape is
// the operator's browser speaking to the BMC through this proxy: the login
// POST, the ticket exchange and the KVM page all originate here under one
// origin, so the vendor flow runs unmodified.
//
// Trust model: the session route requires the mammoth API token (?token= for
// window.open, or the bearer header) and mints a short-lived gate cookie
// (HMAC over machine + expiry, keyed with the API token). The proxy answers
// only gate-cookie'd requests on paths the engine does not own; everything
// else keeps the engine's behavior. BMC credentials ride the launcher page
// once (the operator could read them from the credentials API anyway — same
// trust domain), never stored browser-side beyond that page's lifetime.
package bmcweb

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/store"
)

const (
	// GateCookie carries the console-session binding (machine + expiry +
	// signature). One machine at a time per browser: opening a console for
	// another machine rebinds the cookie, and the previous console's
	// proxied XHRs would follow it — documented v1 limitation.
	GateCookie = "mammoth_bmcweb"
	// gateTTL bounds the whole console window (the BMC's own web session
	// dies at ~45 minutes regardless — docs/compat/huawei.md §会话模型).
	gateTTL = 4 * time.Hour
)

// Proxy is the engine-side BMC web reverse proxy. Nil-safe at the wiring
// site: a nil proxy leaves every engine route untouched. Vendor launchers
// register via Register; machines without a match take the generic fallback
// (plain proxy, manual login).
type Proxy struct {
	Machines    *store.MachineRepo
	Credentials *store.CredentialRepo
	Crypto      *store.SecretCrypto
	BMC         *bmc.Registry // power/virtual-media from the console panel
	apiToken    string
	launchers   []Launcher
	cache       assetCache
}

// SetBMC wires the driver registry for the console panel's power/media
// actions (set after New at the wiring site).
func (p *Proxy) SetBMC(reg *bmc.Registry) { p.BMC = reg }

// assetCache holds proxied STATIC BMC content (ETag-bearing GETs only, never
// /redfish) so the vendor web bundle loads at engine-LAN speed instead of a
// per-file double hop through the BMC — the KVM page pulls ~40 assets and
// the BMC web server answers each in 200-500ms through the proxy (real-run
// waterfall 2026-11-07). Invalidation: the vendor version-stamps its bundles
// in the URL query; a TTL (6h) bounds staleness for unversioned paths; ETag
// revalidation answers If-None-Match locally.
type assetCache struct {
	mu    sync.Mutex
	m     map[string]*cachedResp
	bytes int64
}

const (
	cacheMaxBytes = 256 << 20
	cacheEntryMax = 16 << 20
	cacheTTL      = 6 * time.Hour
)

type cachedResp struct {
	status int
	header http.Header
	body   []byte
	etag   string
	at     time.Time
}

func newAssetCache() assetCache {
	return assetCache{m: make(map[string]*cachedResp)}
}

func (c *assetCache) get(key string) (*cachedResp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cr, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if time.Since(cr.at) > cacheTTL {
		delete(c.m, key)
		c.bytes -= int64(len(cr.body))
		return nil, false
	}
	return cr, true
}

func (c *assetCache) put(key string, cr *cachedResp) {
	if len(cr.body) > cacheEntryMax {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.m[key]; ok {
		c.bytes -= int64(len(old.body))
	}
	c.m[key] = cr
	c.bytes += int64(len(cr.body))
	for c.bytes > cacheMaxBytes { // naive overflow: drop everything (console bundle ~10MB)
		c.m = make(map[string]*cachedResp)
		c.bytes = 0
	}
}

// cacheable reports whether a request/response pair may populate the static
// cache: plain GET, not the Redfish API, no Range, 200 with an ETag.
func cacheable(r *http.Request, resp *http.Response) bool {
	return r.Method == http.MethodGet &&
		!strings.HasPrefix(r.URL.Path, "/redfish/") &&
		r.Header.Get("Range") == "" &&
		resp.StatusCode == http.StatusOK &&
		resp.Header.Get("Etag") != ""
}

func New(machines *store.MachineRepo, credentials *store.CredentialRepo, crypto *store.SecretCrypto, apiToken string) *Proxy {
	return &Proxy{Machines: machines, Credentials: credentials, Crypto: crypto, apiToken: apiToken, cache: newAssetCache()}
}

// Register adds a vendor launcher (first match wins; evaluated in
// registration order).
func (p *Proxy) Register(l Launcher) { p.launchers = append(p.launchers, l) }

func (p *Proxy) launcherFor(m *store.Machine) Launcher {
	for _, l := range p.launchers {
		if l.Matches(m) {
			return l
		}
	}
	return genericFallback{}
}

// Supports reports whether the machine has a registered vendor launcher —
// the engine's /console endpoint returns the bmcweb session URL for such
// machines instead of the driver's synthesized URL (which the vendors'
// launch flow makes unusable anyway, docs/compat/huawei.md §KVM).
func (p *Proxy) Supports(m *store.Machine) bool {
	for _, l := range p.launchers {
		if l.Matches(m) {
			return true
		}
	}
	return false
}

// SessionURL is the launcher link the console opens (token rides the query:
// window.open cannot set headers).
func (p *Proxy) SessionURL(externalBase, machineID string) string {
	return strings.TrimSuffix(externalBase, "/") + "/bmcweb/" + machineID +
		"/session?token=" + url.QueryEscape(p.apiToken)
}

// ── session route: token → gate cookie + launcher page ──────────────────────

// Session validates the operator's token (?token= — window.open cannot set
// headers), resolves the machine's BMC web target and credentials, plants
// the gate cookie, and serves the launcher page that logs the BROWSER into
// the BMC and arms the vendor launch flow.
func (p *Proxy) Session(c *gin.Context, machineID string) {
	if !p.checkToken(c) {
		return
	}
	m, user, pass, ok := p.resolve(c, machineID)
	if !ok {
		return
	}
	// SameSite=Lax: the console window and every vendor XHR are same-origin;
	// Lax keeps cross-site POSTs from carrying the gate.
	c.SetCookie(GateCookie, p.sign(machineID, time.Now().Add(gateTTL).Unix()),
		int(gateTTL.Seconds()), "/", "", false, true)
	// Evict any stale BMC session cookie from the browser jar first: the
	// vendor login POST would otherwise carry it and the BMC answers the
	// login with 400 (real run, 2026-11-06 — cookies are host-scoped and
	// outlive every experiment on shared dev hosts).
	c.SetCookie("SessionId", "", -1, "/", "", false, false)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(p.launcherHTML(m, user, pass)))
}

func (p *Proxy) checkToken(c *gin.Context) bool {
	if p.apiToken == "" {
		problem(c, http.StatusServiceUnavailable, "SCHEMA_AUTH_UNCONFIGURED", "Auth not configured")
		return false
	}
	if tok := c.Query("token"); tok != "" &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(p.apiToken)) == 1 {
		return true
	}
	const prefix = "Bearer "
	if hdr := c.GetHeader("Authorization"); strings.HasPrefix(hdr, prefix) &&
		subtle.ConstantTimeCompare([]byte(hdr[len(prefix):]), []byte(p.apiToken)) == 1 {
		return true
	}
	problem(c, http.StatusUnauthorized, "SCHEMA_UNAUTHORIZED", "missing or invalid token")
	return false
}

func problem(c *gin.Context, status int, code, detail string) {
	c.JSON(status, gin.H{
		"code": code, "title": code, "status": status, "detail": detail,
	})
}

// resolve loads the machine and its BMC credentials. Every failure path
// writes the response.
func (p *Proxy) resolve(c *gin.Context, machineID string) (*store.Machine, string, string, bool) {
	m, err := p.Machines.Get(c.Request.Context(), machineID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "MACHINE_NOT_FOUND", "status": 404, "detail": err.Error()})
		return nil, "", "", false
	}
	if m.BMCProtocol == "none" || m.BMCAddress == "" {
		c.JSON(http.StatusConflict, gin.H{"code": "BMCWEB_NO_SURFACE", "status": 409,
			"detail": "machine has no BMC web surface (protocol " + m.BMCProtocol + ")"})
		return nil, "", "", false
	}
	target := normalizeTarget(m.BMCAddress)
	if target == nil {
		c.JSON(http.StatusConflict, gin.H{"code": "BMCWEB_BAD_ADDRESS", "status": 409,
			"detail": "unusable BMC address: " + m.BMCAddress})
		return nil, "", "", false
	}
	if m.BMCCredentialID == "" {
		c.JSON(http.StatusConflict, gin.H{"code": "BMCWEB_NO_CREDENTIAL", "status": 409,
			"detail": "machine has no BMC credential registered"})
		return nil, "", "", false
	}
	row, err := p.Credentials.Get(c.Request.Context(), m.BMCCredentialID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "CREDENTIAL_NOT_FOUND", "status": 404, "detail": err.Error()})
		return nil, "", "", false
	}
	plain, err := p.Crypto.Decrypt(row.SecretEncrypted)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "CREDENTIAL_DECRYPT_FAILED", "status": 500})
		return nil, "", "", false
	}
	var secret struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(plain, &secret); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "CREDENTIAL_UNREADABLE", "status": 500})
		return nil, "", "", false
	}
	return m, secret.Username, secret.Password, true
}

// normalizeTarget parses a stored BMC address into an upstream URL. Bare
// hosts default to https (the vendor default; http addresses may be stored
// with an explicit scheme).
func normalizeTarget(addr string) *url.URL {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil // fake:// and friends have no web surface
	}
	if u.Port() == "" {
		u.Host = u.Host + ":443"
	}
	return u
}

// ── gate cookie ─────────────────────────────────────────────────────────────

func (p *Proxy) sign(machineID string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(p.apiToken))
	fmt.Fprintf(mac, "%s|%d", machineID, exp)
	return machineID + "." + strconv.FormatInt(exp, 10) + "." +
		hex.EncodeToString(mac.Sum(nil))
}

func (p *Proxy) verify(machineID, expStr, sig string) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, []byte(p.apiToken))
	fmt.Fprintf(mac, "%s|%d", machineID, exp)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

// MachineFromGate reads the gate cookie from an inbound proxied request.
func (p *Proxy) MachineFromGate(r *http.Request) string {
	ck, err := r.Cookie(GateCookie)
	if err != nil || ck.Value == "" {
		return ""
	}
	parts := strings.SplitN(ck.Value, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	if !p.verify(parts[0], parts[1], parts[2]) {
		return ""
	}
	return parts[0]
}

// ── the proxy half (NoRoute catch-all) ──────────────────────────────────────

// TryProxy serves one request through the BMC proxy when a valid gate cookie
// is present; false keeps the caller's fallback (the engine's notFound).
// The BMC web app uses ABSOLUTE asset/XHR paths (/redfish, /update, /theme,
// /src, /lib, /bmc, ...) — a path-prefix proxy cannot contain it, so the
// gate cookie (not the path) names the machine and engine-owned routes
// simply take precedence by matching first.
func (p *Proxy) TryProxy(w http.ResponseWriter, r *http.Request) bool {
	id := p.MachineFromGate(r)
	if id == "" {
		return false
	}
	m, err := p.Machines.Get(r.Context(), id)
	if err != nil || m.BMCProtocol == "none" || m.BMCAddress == "" {
		http.Error(w, "console session target unavailable", http.StatusGone)
		return true
	}
	target := normalizeTarget(m.BMCAddress)
	if target == nil {
		http.Error(w, "unusable BMC address: "+m.BMCAddress, http.StatusGone)
		return true
	}
	// static-asset cache fast path: ETag-bearing GETs answered from the
	// engine (If-None-Match → local 304), zero BMC roundtrip.
	cacheKey := ""
	if r.Method == http.MethodGet &&
		!strings.HasPrefix(r.URL.Path, "/redfish/") &&
		r.Header.Get("Range") == "" {
		cacheKey = id + "|" + r.URL.RequestURI()
		inm := r.Header.Get("If-None-Match")
		if cr, ok := p.cache.get(cacheKey); ok {
			h := w.Header()
			for k, vs := range cr.header {
				for _, v := range vs {
					h.Add(k, v)
				}
			}
			// ETag revalidation: the browser's heuristic cache revalidates
			// against the served ETag even with no Cache-Control upstream.
			if inm != "" && etagMatches(inm, cr.etag) {
				w.WriteHeader(http.StatusNotModified)
				return true
			}
			w.WriteHeader(cr.status)
			_, _ = w.Write(cr.body)
			return true
		}
	}
	rp := &httputil.ReverseProxy{
		Transport: &http.Transport{
			// Vendor self-signed cert + the same wide TLS floor the redfish
			// driver carries: HDM (AMI MegaRAC) negotiates only RSA-key-exchange
			// suites that Go defaults no longer offer (docs/compat/h3c.md §1).
			TLSClientConfig:   tlsVendorConfig(),
			ForceAttemptHTTP2: false,
		},
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Host rewrite is load-bearing: the BMC derives the KVM ticket's
			// server address (jnlp IPA/IPB) from the request Host header —
			// without it the H5 client dials a dead wss target
			// (docs/compat/huawei.md, 双向实测 2026-11-06). The 2026-11-06 E2E
			// "sessions die with the rewrite" read was later falsified: the
			// session table was saturated from repeated experiments, killing
			// every new session regardless of the proxy's Host handling.
			req.Host = target.Host
			req.URL.RawPath = ""
		},
		ModifyResponse: func(resp *http.Response) error {
			stripSecureCookie(resp)
			// populate the static cache (cacheKey non-empty ⟺ cacheable
			// request shape; cacheable() re-checks the response itself).
			if cacheKey != "" && cacheable(r, resp) {
				body, err := io.ReadAll(resp.Body)
				if err == nil {
					resp.Body = io.NopCloser(bytes.NewReader(body))
					resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
					p.cache.put(cacheKey, &cachedResp{
						status: resp.StatusCode,
						header: resp.Header.Clone(),
						body:   body,
						etag:   resp.Header.Get("Etag"),
						at:     time.Now(),
					})
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "bmcweb proxy: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
	return true
}

// etagMatches evaluates an If-None-Match header against the cached ETag
// (exact match or any-match "*"; weak prefixes compare on the opaque part).
func etagMatches(inm, etag string) bool {
	if inm == "*" {
		return true
	}
	trim := func(s string) string {
		s = strings.TrimSpace(s)
		return strings.TrimPrefix(strings.TrimPrefix(s, "W/"), `"`)
	}
	for _, cand := range strings.Split(inm, ",") {
		if trim(cand) == trim(etag) {
			return true
		}
	}
	return false
}

// tlsVendorConfig is the shared TLS floor for proxying to vendor BMCs:
// self-signed certs, TLS 1.0 floor, every suite (legacy BMC stacks — AMI
// MegaRAC family — need RSA-key-exchange ciphers Go stopped offering by
// default).
func tlsVendorConfig() *tls.Config {
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		suites = append(suites, s.ID)
	}
	return &tls.Config{ // #nosec G402 — vendor self-signed certs are the norm
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		CipherSuites:       suites,
	}
}

// stripSecureCookie drops the Secure attribute from upstream cookies: the
// BMC ships `Secure` unconditionally, and a plain-HTTP engine origin would
// silently drop the session cookie (localhost's secure-context exemption
// does not apply to a real engine address).
func stripSecureCookie(resp *http.Response) {
	values := resp.Header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	resp.Header.Del("Set-Cookie")
	for _, v := range values {
		parts := strings.Split(v, ";")
		kept := parts[:1]
		for _, attr := range parts[1:] {
			attr = strings.TrimSpace(attr)
			if strings.EqualFold(attr, "Secure") {
				continue
			}
			kept = append(kept, attr)
		}
		resp.Header.Add("Set-Cookie", strings.Join(kept, "; "))
	}
}
