// H3C HDM web-API helpers for the redfish driver: the hardware SEL is only
// fully exposed through HDM's own web layer (GET /api/logs/event — one
// JSON array, 1569 records in ~2s on the real unit), while the Redfish
// LogServices carry HDM's operational logs (audit/firmware/config) and
// answer the hardware records unreliably (docs/compat/h3c.md §D3).
//
// The web session is the RSA one (real web login, operator F12 capture
// 2026-11-14): GET /api/rsapublickey → POST /api/session with the
// credentials PKCS#1 v1.5 encrypted under that key (public-key params
// echoed back as cookies — the server picks its private key by cookie),
// and every /api/* call afterwards carries header X-CSRFTOKEN with the
// login response's CSRFToken.

package redfish

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// h3cWebClient is one HDM web-API session (RSA login + CSRF header).
type h3cWebClient struct {
	base string
	http *http.Client
	csrf string
}

func newH3CWebClient(addr string, timeout time.Duration) *h3cWebClient {
	a := strings.TrimSuffix(strings.TrimSpace(addr), "/")
	if !strings.Contains(a, "://") {
		a = "https://" + a
	}
	jar, _ := cookiejar.New(nil)
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		suites = append(suites, s.ID)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &h3cWebClient{
		base: a,
		http: &http.Client{
			Jar:     jar,
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ // #nosec G402 — BMC self-signed certs
					InsecureSkipVerify: true,
					MinVersion:         tls.VersionTLS10,
					CipherSuites:       suites,
				},
			},
		},
	}
}

// login performs the RSA session dance and stores the CSRF token. One
// attempt per call (HDM counts failures).
func (c *h3cWebClient) login(user, pass string) error {
	resp, err := c.http.Get(c.base + "/api/rsapublickey")
	if err != nil {
		return fmt.Errorf("h3c: rsapublickey: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var pk struct {
		Exponent string `json:"exponent"`
		Modulus  string `json:"modulus"`
		RSAValue int    `json:"RSA_Value"`
	}
	if err := json.Unmarshal(body, &pk); err != nil {
		return fmt.Errorf("h3c: rsapublickey: %w (%.120s)", err, body)
	}
	n := new(big.Int)
	if _, ok := n.SetString(pk.Modulus, 16); !ok {
		return fmt.Errorf("h3c: rsapublickey: bad modulus")
	}
	e := new(big.Int)
	if _, ok := e.SetString(pk.Exponent, 16); !ok {
		return fmt.Errorf("h3c: rsapublickey: bad exponent")
	}
	pub := &rsa.PublicKey{N: n, E: int(e.Int64())}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(user))
	if err != nil {
		return err
	}
	ctP, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(pass))
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("username", base64.StdEncoding.EncodeToString(ct))
	form.Set("password", base64.StdEncoding.EncodeToString(ctP))
	form.Set("log_type", "1")
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/session", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// The server picks its private key by these cookie params — echo the
	// public key back verbatim.
	req.AddCookie(&http.Cookie{Name: "MODULUS", Value: pk.Modulus})
	req.AddCookie(&http.Cookie{Name: "EXPONENT", Value: pk.Exponent})
	req.AddCookie(&http.Cookie{Name: "RSA_Value", Value: fmt.Sprint(pk.RSAValue)})
	resp, err = c.http.Do(req)
	if err != nil {
		return fmt.Errorf("h3c: session: %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var lr struct {
		CC        int    `json:"cc"`
		CSRFToken string `json:"CSRFToken"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return fmt.Errorf("h3c: login: unexpected response (%.120s)", body)
	}
	if lr.CC != 0 {
		return fmt.Errorf("h3c: login failed (cc=%d)", lr.CC)
	}
	c.csrf = lr.CSRFToken
	return nil
}

// get performs one authenticated web-API call (X-CSRFTOKEN header rides
// every call after login).
func (c *h3cWebClient) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-CSRFTOKEN", c.csrf)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("h3c: GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

// h3cEvent is one /api/logs/event record (IPMI SEL shape, newest first).
type h3cEvent struct {
	ID               int    `json:"id"`
	Timestamp        int64  `json:"timestamp"`
	Level            int    `json:"level"`
	EventDescription string `json:"event_description"`
	SensorName       string `json:"sensor_name"`
	SensorType       string `json:"sensor_type"`
	EventDirection   string `json:"event_direction"`
}

// h3cSEL fetches the hardware event log through the HDM web API.
func h3cSEL(addr, user, pass string, timeout time.Duration) ([]bmc.SELEntry, error) {
	c := newH3CWebClient(addr, timeout)
	if err := c.login(user, pass); err != nil {
		return nil, err
	}
	raw, err := c.get("/api/logs/event")
	if err != nil {
		return nil, err
	}
	var events []h3cEvent
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("h3c: sel: %w", err)
	}
	// The array is newest-first; keep the head within the same cap the
	// Redfish walk applies.
	if len(events) > bmc.SELMaxEntries {
		events = events[:bmc.SELMaxEntries]
	}
	out := make([]bmc.SELEntry, 0, len(events))
	for _, ev := range events {
		out = append(out, bmc.SELEntry{
			ID:        fmt.Sprint(ev.ID),
			Timestamp: time.Unix(ev.Timestamp, 0).UTC().Format(time.RFC3339),
			Severity:  h3cLevel(ev.Level),
			Message:   strings.TrimSpace(ev.SensorName + " " + ev.EventDescription),
		})
	}
	return out, nil
}

// h3cLevel maps the HDM event level to the RFB-ish severity vocabulary the
// driver reports (values observed on the real unit: 1 = informational).
func h3cLevel(level int) string {
	switch level {
	case 1:
		return "ok"
	case 2:
		return "warning"
	case 3:
		return "critical"
	default:
		return "unknown"
	}
}
