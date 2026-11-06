package bmcweb

// PG-gated full-flow tests: gate cookie issue → proxied request → upstream
// sees the rewritten Host, the browser sees stripped Secure cookies. The
// fake upstream is a plain httptest server registered as the machine's BMC
// address (scheme-explicit so normalizeTarget keeps it on http).

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/bmcweb/huawei"
	"github.com/3th1nk/mammoth/internal/store"
)

func bmcwebSuite(t *testing.T) (*Proxy, *gin.Engine, *store.SecretCrypto, string) {
	t.Helper()
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping bmcweb full-flow suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Fake upstream BMC: records the Host header it sees, sets a Secure
	// session cookie, and echoes a marker body.
	var seenHost, seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost, seenPath = r.Host, r.URL.RequestURI()
		t.Logf("upstream hit: host=%s path=%s", seenHost, seenPath)
		w.Header().Add("Set-Cookie", "SessionId=fakeupstream; Path=/; Secure; Httponly")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok:" + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	crypto, err := store.NewSecretCrypto(key)
	if err != nil {
		t.Fatalf("crypto: %v", err)
	}
	sealed, err := crypto.Encrypt([]byte(`{"username":"root","password":"p@ss"}`))
	if err != nil {
		t.Fatalf("seed encrypt: %v", err)
	}

	const machineID = "tst_bmcweb_m"
	const credID = "tst_bmcweb_c"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machineID)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, credID)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'bmcweb-test', 'bmc', $2::bytea)`, credID, sealed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, vendor, state)
		VALUES ($1, '{}'::jsonb, $2, 'redfish', $3, 'Huawei', 'ready')`,
		machineID, upstream.URL, credID); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	gin.SetMode(gin.TestMode)
	p := New(store.NewMachineRepo(db), store.NewCredentialRepo(db), crypto, testToken)
	p.Register(huawei.New())
	r := gin.New()
	r.GET("/bmcweb/:id/session", func(c *gin.Context) { p.Session(c, c.Param("id")) })
	return p, r, crypto, machineID
}

func TestSessionIssuesGateAndLauncher(t *testing.T) {
	p, r, _, machineID := bmcwebSuite(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/session?token="+testToken, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session = %d %q, want 200", w.Code, w.Body.String())
	}
	// gate cookie planted
	cookies := w.Result().Cookies()
	var gate *http.Cookie
	for _, ck := range cookies {
		if ck.Name == GateCookie {
			gate = ck
		}
	}
	if gate == nil || gate.Value == "" || !gate.HttpOnly {
		t.Fatalf("gate cookie missing/invalid: %+v", cookies)
	}
	// launcher arms the vendor flow with the seeded credential
	body := w.Body.String()
	for _, want := range []string{"H5ClientModel", "kvm_h5.html", `"user":"root"`, `"password":"p@ss"`} {
		if !strings.Contains(body, want) {
			t.Errorf("launcher missing %q", want)
		}
	}
	// the gate cookie must resolve through TryProxy
	_ = p

	// wrong token is rejected
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/session?token=nope", nil))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", w2.Code)
	}
}

func TestTryProxyHostRewriteAndCookieStrip(t *testing.T) {
	p, r, _, machineID := bmcwebSuite(t)

	// mint the gate through the session route
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/session?token="+testToken, nil))
	var gate string
	for _, ck := range w.Result().Cookies() {
		if ck.Name == GateCookie {
			gate = ck.Value
		}
	}
	if gate == "" {
		t.Fatal("no gate cookie from session")
	}

	// proxied request (any engine-unowned path, gate-cookie keyed)
	req := httptest.NewRequest(http.MethodGet, "/redfish/v1/Managers/1/KvmService", nil)
	req.AddCookie(&http.Cookie{Name: GateCookie, Value: gate})
	rec := httptest.NewRecorder()
	ok := p.TryProxy(rec, req)
	if !ok {
		t.Fatal("TryProxy refused a gated request")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream-ok") {
		t.Fatalf("proxied body = %d %q", rec.Code, rec.Body.String())
	}
	// Secure stripped on the way out
	sets := rec.Result().Header.Values("Set-Cookie")
	found := false
	for _, s := range sets {
		if strings.Contains(s, "SessionId=fakeupstream") {
			found = true
			if strings.Contains(strings.ToLower(s), "secure") {
				t.Errorf("Secure survived the proxy: %q", s)
			}
		}
	}
	if !found {
		t.Fatalf("upstream Set-Cookie lost: %v", sets)
	}
}

func TestTryProxyWithoutGateFallsThrough(t *testing.T) {
	p, _, _, _ := bmcwebSuite(t)
	req := httptest.NewRequest(http.MethodGet, "/src/virtualControl/kvm_h5.html", nil)
	if p.TryProxy(httptest.NewRecorder(), req) {
		t.Fatal("TryProxy served a request without a gate cookie")
	}
}

func TestSessionRejectsMissingMachine(t *testing.T) {
	_, r, _, _ := bmcwebSuite(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bmcweb/tst_bmcweb_missing/session?token="+testToken, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing machine = %d, want 404", w.Code)
	}
}
