package bmcweb

// PG-gated Dell console-panel tests: the engine-owned KVM routes (gate
// cookie keyed) and the vendored noVNC assets. The websocket's console-side
// flow (login → ticket → 5900) is exercised against real hardware in the
// E2E runbook; here we verify routing, auth and asset serving.

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/bmcweb/dell"
	"github.com/3th1nk/mammoth/internal/store"
)

func dellSuite(t *testing.T) (*Proxy, *gin.Engine, string) {
	t.Helper()
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping dell console suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The BMC address is scheme-explicit http so the fake upstream stays on
	// plain HTTP; the console flow itself is not exercised here.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
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

	const machineID = "tst_dell_kvm_m"
	const credID = "tst_dell_kvm_c"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machineID)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, credID)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'dell-kvm-test', 'bmc', $2::bytea)`, credID, sealed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, vendor, state)
		VALUES ($1, '{}'::jsonb, $2, 'redfish', $3, 'dell inc.', 'ready')`,
		machineID, upstream.URL, credID); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	gin.SetMode(gin.TestMode)
	p := New(store.NewMachineRepo(db), store.NewCredentialRepo(db), crypto, testToken)
	p.Register(dell.New())
	if !p.Supports(&store.Machine{ID: machineID, BMCProtocol: "redfish", BMCAddress: upstream.URL,
		Vendor: strPtr("Dell Inc.")}) {
		t.Fatal("dell launcher did not match a dell vendor machine")
	}
	if p.Supports(&store.Machine{ID: machineID, BMCProtocol: "redfish", BMCAddress: upstream.URL,
		Vendor: strPtr("Huawei")}) {
		t.Fatal("dell launcher matched a huawei machine")
	}
	r := gin.New()
	r.GET("/bmcweb/:id/session", func(c *gin.Context) { p.Session(c, c.Param("id")) })
	r.NoRoute(func(c *gin.Context) {
		if p.TryKVM(c) || p.ServeAssets(c, c.Request.URL.Path) {
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "status": 404})
	})
	return p, r, machineID
}

func strPtr(s string) *string { return &s }

// mintGate gets a gate cookie for the machine through the session route.
func mintGate(t *testing.T, r *gin.Engine, machineID string) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/session?token="+testToken, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("session = %d %q", w.Code, w.Body.String())
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == GateCookie {
			return ck.Value
		}
	}
	t.Fatal("no gate cookie")
	return ""
}

func TestDellKVMPageServesNoVNCShell(t *testing.T) {
	_, r, machineID := dellSuite(t)
	gate := mintGate(t, r, machineID)

	req := httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/kvm", nil)
	req.AddCookie(&http.Cookie{Name: GateCookie, Value: gate})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("kvm page = %d %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"rfb.js", "/bmcweb/" + machineID + "/kvm/ws", "scaleViewport"} {
		if !strings.Contains(body, want) {
			t.Errorf("kvm page missing %q", want)
		}
	}
}

func TestDellKVMPageRequiresGate(t *testing.T) {
	_, r, machineID := dellSuite(t)
	req := httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/kvm", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("kvm page without gate = %d, want 401", w.Code)
	}
	// Gate for another machine must not open this one.
	otherGate := mintGate(t, r, machineID) // rebind: same machine here
	req = httptest.NewRequest(http.MethodGet, "/bmcweb/tst_dell_kvm_missing/kvm", nil)
	req.AddCookie(&http.Cookie{Name: GateCookie, Value: otherGate})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("kvm page for missing machine with valid gate = %d, want 401", w.Code)
	}
}

func TestDellKVMWSRejectsWithoutGate(t *testing.T) {
	_, r, machineID := dellSuite(t)
	// resolve() passes (machine + credential exist), but the gate check
	// answers before any websocket upgrade happens.
	req := httptest.NewRequest(http.MethodGet, "/bmcweb/"+machineID+"/kvm/ws", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("kvm ws without gate = %d, want 401", w.Code)
	}
}

func TestDellNoVNCAssetsServe(t *testing.T) {
	_, r, _ := dellSuite(t)
	for _, p := range []string{"/bmcweb/assets/novnc/core/rfb.js", "/bmcweb/assets/novnc/core/websock.js"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", p, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("%s content-type = %q", p, ct)
		}
	}
	// Traversal is refused.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bmcweb/assets/novnc/../secrets", nil))
	if w.Code == http.StatusOK {
		t.Fatal("traversal served")
	}
}
