// Dell iDRAC legacy web API (iDRAC6/7/8): the /data form-login that mints
// the web session and the viewer.jnlp download that yields the one-time
// console credentials for port 5900.
//
// Wire facts verified against a real iDRAC7 2.65 (PowerEdge R620,
// 2026-11-13, docs/compat/dell.md §console): the login POST answers 200 with
// an XML body carrying forwardUrl "index.html?ST1=..,ST2=.." — ST1 names the
// console JNLP, ST2 rides every later /data request as a request header (an
// omitted ST2 answers 401 to authenticated endpoints). Static UI assets live
// behind a broken web root on that unit (404s regardless of auth) which does
// not affect this API surface.
//
// Concurrency: a Client is one iDRAC web session — not safe for concurrent
// use. The KVM bridge owns one per console session and logs out on close
// (iDRACs cap concurrent web sessions; docs/compat/huawei.md §会话模型).

package dell

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ConsoleCredentials is what viewer.jnlp passes to the Avocent viewer:
// host/ports plus the one-time encoded credentials the console login wants
// (they are NOT the web login's plaintext — iDRAC mints per-launch values).
type ConsoleCredentials struct {
	Host     string
	KMPort   int // keyboard/mouse channel (5900)
	VPort    int // video channel (5900)
	User     string
	Password string
	APCP     bool
	Title    string
}

// Client is one iDRAC web session (cookie jar + ST1/ST2 tokens).
type Client struct {
	base    string // https://host[:port]
	user    string
	pass    string
	http    *http.Client
	st1     string
	st2     string
	forward string
}

// NewClient builds a session client for the BMC web API. addr may carry a
// scheme; bare hosts default to https. The TLS floor is TLS 1.0 with all
// cipher suites offered — iDRAC6/7 ship ancient stacks (1024-bit RSA,
// 3DES) that modern Go defaults no longer negotiate.
func NewClient(addr, user, pass string, timeout time.Duration) (*Client, error) {
	a := strings.TrimSpace(addr)
	if !strings.Contains(a, "://") {
		a = "https://" + a
	}
	u, err := url.Parse(a)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bmc address %q unusable", addr)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		suites = append(suites, s.ID)
	}
	return &Client{
		base: strings.TrimRight(a, "/"),
		user: user,
		pass: pass,
		http: &http.Client{
			Jar:     jar,
			Timeout: timeout,
			// iDRAC answers with 302 instead of 401 on some endpoints; never
			// follow — the status code is signal.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // iDRAC certs are self-signed
					MinVersion:         tls.VersionTLS10,
					MaxVersion:         tls.VersionTLS12,
					CipherSuites:       suites,
				},
				MaxIdleConnsPerHost: 1,
			},
		},
	}, nil
}

// LoginError carries the iDRAC's authResult code (login attempts are
// counted server-side — callers must not retry automatically).
type LoginError struct {
	AuthResult   int
	BlockingTime int // seconds the iDRAC wants the client to wait
	Message      string
}

func (e *LoginError) Error() string {
	desc := map[int]string{
		1: "invalid credentials", 2: "missing username", 3: "missing password",
		4: "insufficient privilege", 5: "session limit reached", 99: "login failed",
	}[e.AuthResult]
	if desc == "" {
		desc = "authResult " + strconv.Itoa(e.AuthResult)
	}
	if e.BlockingTime > 0 {
		desc += fmt.Sprintf(" (blocked for %ds)", e.BlockingTime)
	}
	if e.Message != "" {
		desc += ": " + e.Message
	}
	return "dell login: " + desc
}

type loginXML struct {
	Status       string `xml:"status"`
	AuthResult   int    `xml:"authResult"`
	BlockingTime int    `xml:"blockingTime"`
	ForwardURL   string `xml:"forwardUrl"`
	ErrorMsg     string `xml:"errorMsg"`
	ST1Field     string `xml:"ST1"`
	ST2Field     string `xml:"ST2"`
}

var st1Re = regexp.MustCompile(`ST1=([A-Za-z0-9+/=_-]+)`)
var st2Re = regexp.MustCompile(`ST2=([A-Za-z0-9+/=_-]+)`)

// Login performs the form login. One attempt per call — iDRACs lock accounts
// after repeated failures.
func (c *Client) Login(ctx context.Context) error {
	// The iDRAC parses the form positionally — keep the browser's order
	// (user first, password second; url.Values.Encode would alphabetise).
	form := "user=" + url.QueryEscape(c.user) + "&password=" + url.QueryEscape(c.pass)
	resp, body, err := c.do(ctx, http.MethodPost, "/data/login", form, "")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dell login: HTTP %d", resp.StatusCode)
	}
	var lr loginXML
	if err := xml.Unmarshal(body, &lr); err != nil {
		return fmt.Errorf("dell login: unexpected response: %w (%.200s)", err, body)
	}
	if lr.AuthResult != 0 {
		return &LoginError{AuthResult: lr.AuthResult, BlockingTime: lr.BlockingTime, Message: lr.ErrorMsg}
	}
	c.forward = lr.ForwardURL
	// Token placement varies by firmware: standalone <ST1>/<ST2> elements, or
	// both only inside forwardUrl ("index.html?ST1=..,ST2=.."). Cover all.
	c.st1, c.st2 = lr.ST1Field, lr.ST2Field
	if c.st1 == "" {
		if m := st1Re.FindStringSubmatch(lr.ForwardURL); m != nil {
			c.st1 = m[1]
		}
	}
	if c.st2 == "" {
		if m := st2Re.FindStringSubmatch(lr.ForwardURL); m != nil {
			c.st2 = m[1]
		}
	}
	if c.st1 == "" {
		return fmt.Errorf("dell login: no ST1 token in response (%.200s)", body)
	}
	return nil
}

// ForwardURL is the page the login asked the browser to load (empty before
// Login).
func (c *Client) ForwardURL() string { return c.forward }

// Logout ends the web session (GET /data/logout). A 302/401 answer means the
// session is already gone — still a successful logout.
func (c *Client) Logout(ctx context.Context) error {
	resp, _, err := c.do(ctx, http.MethodGet, "/data/logout", "", "")
	if err != nil {
		return err
	}
	_ = resp
	return nil
}

// ConsoleLaunch fetches and parses viewer.jnlp for this session: the URL is
// the one the web UI builds — /viewer.jnlp(host@0@title@millis@ST1=<st1>)
// with the title form-urlencoded (spaces as '+'). Referer rides the UI page.
func (c *Client) ConsoleLaunch(ctx context.Context, title string) (*ConsoleCredentials, error) {
	host := c.host()
	name := fmt.Sprintf("%s@0@%s@%d@ST1=%s", host, url.QueryEscape(title), time.Now().UnixMilli(), c.st1)
	referer := fmt.Sprintf("https://%s/index.html", host)
	if c.forward != "" {
		referer = fmt.Sprintf("https://%s/%s", host, c.forward)
	}
	resp, body, err := c.do(ctx, http.MethodGet, "/viewer.jnlp("+name+")", "", referer)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("dell console: jnlp fetch rejected (session expired?)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dell console: jnlp fetch HTTP %d", resp.StatusCode)
	}
	return ParseJNLP(body, host)
}

// ParseJNLP extracts the viewer arguments from a JNLP document. fallbackHost
// stands in when the JNLP carries no ip= argument.
func ParseJNLP(body []byte, fallbackHost string) (*ConsoleCredentials, error) {
	var doc struct {
		Args []string `xml:"application-desc>argument"`
	}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("dell jnlp: %w", err)
	}
	cc := &ConsoleCredentials{KMPort: 5900, VPort: 5900, Host: fallbackHost}
	for _, a := range doc.Args {
		k, v, _ := strings.Cut(strings.TrimSpace(a), "=")
		switch k {
		case "ip":
			cc.Host = v
		case "kmport":
			cc.KMPort, _ = strconv.Atoi(v)
		case "vport":
			cc.VPort, _ = strconv.Atoi(v)
		case "user":
			cc.User = v
		case "passwd", "password":
			cc.Password = v
		case "apcp":
			cc.APCP = v == "1" || v == "true"
		case "title":
			cc.Title = v
		}
	}
	if cc.Host == "" || cc.User == "" || cc.Password == "" {
		return nil, fmt.Errorf("dell jnlp: missing ip/user/passwd (%d arguments)", len(doc.Args))
	}
	return cc, nil
}

func (c *Client) host() string {
	u, err := url.Parse(c.base)
	if err != nil {
		return c.base
	}
	return u.Hostname()
}

// do performs one request with the browser-ish header set the iDRAC's web
// server expects (a bare GET with no UA/Accept-Language answered 404 even
// for authenticated paths — real-run quirk, docs/compat/dell.md §静态资源).
func (c *Client) do(ctx context.Context, method, path, body, referer string) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// ST2 rides every authenticated /data call; pre-login calls carry no
	// token yet.
	if len(c.st2) >= 8 {
		req.Header.Set("ST2", c.st2)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("dell web %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, gzerr := gzip.NewReader(resp.Body)
		if gzerr != nil {
			return resp, nil, gzerr
		}
		defer gz.Close()
		out = gz
	}
	data, err := io.ReadAll(io.LimitReader(out, 32<<20))
	if err != nil {
		return resp, nil, err
	}
	return resp, data, nil
}

// randomClientRandom mints the console session id sent in the avct Login
// Request (the viewer uses a value < 1e7).
func randomClientRandom() int32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int32(binary.BigEndian.Uint32(b[:]) % 10_000_000)
}
