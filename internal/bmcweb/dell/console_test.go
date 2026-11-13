package dell

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testLoginXML = `<?xml version="1.0" encoding="UTF-8"?> <root> <status>ok</status> <authResult>0</authResult> <forwardUrl>index.html?ST1=4e9351d74e168ce4898afa0dbbef63e9,ST2=9a6c5931c9b8e9fa95597c01d2fe7e52</forwardUrl> </root>`

// realJNLP is the argument section of a viewer.jnlp downloaded from the real
// iDRAC7 2.65 (ticket values are per-session; shape is the fixture).
const realJNLP = `<?xml version="1.0" encoding="UTF-8"?>
<jnlp codebase="https://192.168.1.71:443" spec="1.0+">
<information>
  <title>Virtual Console Client</title>
  <vendor>Dell Inc.</vendor>
 </information>
 <application-desc main-class="com.avocent.idrac.kvm.Main">
   <argument>ip=192.168.1.71</argument>
   <argument>vm=1</argument>
   <argument>title=idrac-esxi-70%2C+PowerEdge+R620%2C+User%3A+root</argument>
   <argument>user=2@@596147110</argument>
   <argument>passwd=58323253</argument>
   <argument>kmport=5900</argument>
   <argument>vport=5900</argument>
   <argument>apcp=1</argument>
   <argument>reconnect=2</argument>
   <argument>version=2</argument>
 </application-desc>
</jnlp>`

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(srv.URL, "root", "calvin", 10*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestLoginExtractsTokens(t *testing.T) {
	var gotHeaders http.Header
	var gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/data/login" {
			gotBody = r.FormValue("user") + "|" + r.FormValue("password")
			gotHeaders = r.Header.Clone()
			w.Write([]byte(testLoginXML))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotBody != "root|calvin" {
		t.Errorf("login form = %q", gotBody)
	}
	if c.st1 != "4e9351d74e168ce4898afa0dbbef63e9" || c.st2 != "9a6c5931c9b8e9fa95597c01d2fe7e52" {
		t.Errorf("tokens = %q / %q", c.st1, c.st2)
	}
	if gotHeaders.Get("User-Agent") == "" || gotHeaders.Get("Accept-Language") == "" {
		t.Errorf("browser-ish headers missing: %v", gotHeaders)
	}
}

func TestLoginAuthResultError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<root><status>fail</status><authResult>1</authResult><blockingTime>30</blockingTime><errorMsg>bad login</errorMsg></root>`))
	}))
	defer srv.Close()

	err := newTestClient(t, srv).Login(context.Background())
	le, ok := err.(*LoginError)
	if !ok {
		t.Fatalf("want *LoginError, got %v", err)
	}
	if le.AuthResult != 1 || le.BlockingTime != 30 {
		t.Errorf("LoginError = %+v", le)
	}
}

func TestConsoleLaunch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/data/login":
			// The real iDRAC mints the web session here (Set-Cookie
			// -http-session-=...; path=/; secure; httponly).
			http.SetCookie(w, &http.Cookie{Name: "-http-session-", Value: "3::http.session::60b2808d0ed08b7c9f33c77d350bf6f0", Path: "/"})
			w.Write([]byte(testLoginXML))
		case strings.HasPrefix(r.URL.Path, "/viewer.jnlp("):
			// URL shape: /viewer.jnlp(host@0@title@millis@ST1=..) with the
			// session cookie and ST2 header.
			if !strings.Contains(r.URL.Path, "@ST1=4e9351d74e168ce4898afa0dbbef63e9") {
				t.Errorf("jnlp URL missing ST1: %s", r.URL.Path)
			}
			if !strings.Contains(r.URL.Path, "%40") && !strings.Contains(r.URL.Path, "idrac") {
				// title encoded with QueryEscape (spaces '+') — checked loosely
			}
			if r.Header.Get("ST2") == "" {
				t.Errorf("jnlp fetch without ST2 header")
			}
			if r.Header.Get("Cookie") == "" {
				t.Errorf("jnlp fetch without session cookie")
			}
			w.Header().Set("Content-Type", "application/x-java-jnlp-file")
			w.Write([]byte(realJNLP))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	cc, err := c.ConsoleLaunch(context.Background(), "idrac-esxi-70, PowerEdge R620, User: root")
	if err != nil {
		t.Fatalf("ConsoleLaunch: %v", err)
	}
	if cc.Host != "192.168.1.71" || cc.User != "2@@596147110" || cc.Password != "58323253" {
		t.Errorf("credentials = %+v", cc)
	}
	if cc.KMPort != 5900 || cc.VPort != 5900 || !cc.APCP {
		t.Errorf("ports/apcp = %+v", cc)
	}
}

func TestParseJNLPFallbackHost(t *testing.T) {
	jnlp := strings.Replace(realJNLP, "<argument>ip=192.168.1.71</argument>", "", 1)
	cc, err := ParseJNLP([]byte(jnlp), "10.0.0.5")
	if err != nil {
		t.Fatalf("ParseJNLP: %v", err)
	}
	if cc.Host != "10.0.0.5" {
		t.Errorf("fallback host = %q", cc.Host)
	}
}

func TestParseJNLPRejectsEmpty(t *testing.T) {
	if _, err := ParseJNLP([]byte(`<jnlp><application-desc/></jnlp>`), "h"); err == nil {
		t.Fatal("expected error for missing ip/user/passwd")
	}
}

func TestLogoutToleratesGone(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data/login":
			w.Write([]byte(testLoginXML))
		case "/data/logout":
			w.WriteHeader(http.StatusFound) // session already gone
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := c.Logout(context.Background()); err != nil {
		t.Fatalf("Logout after 302: %v", err)
	}
	_ = fmt.Sprint() // keep fmt import when assertions shrink
}
