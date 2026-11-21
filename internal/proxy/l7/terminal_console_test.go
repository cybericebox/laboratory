package l7

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-logr/logr"
	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
)

// memSink collects every log line, key-value pairs included.
type memSink struct {
	mu    sync.Mutex
	lines []string
}

func (m *memSink) Init(logr.RuntimeInfo) {}
func (m *memSink) Enabled(int) bool      { return true }
func (m *memSink) Info(_ int, msg string, kv ...any) {
	m.mu.Lock()
	m.lines = append(m.lines, fmt.Sprint(msg, kv))
	m.mu.Unlock()
}
func (m *memSink) Error(err error, msg string, kv ...any) { m.Info(0, msg+" "+err.Error(), kv...) }
func (m *memSink) WithValues(...any) logr.LogSink         { return m }
func (m *memSink) WithName(string) logr.LogSink           { return m }
func (m *memSink) all() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.lines, "\n")
}

var sessionBlockRE = regexp.MustCompile(`<script type="application/json" id="session">(.*?)</script>`)

// consoleToken reads the console session token out of the page.
func consoleToken(t *testing.T, body string) string {
	t.Helper()
	m := sessionBlockRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no session block in the page: %.300s", body)
	}
	var s struct{ Token string }
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil || s.Token == "" {
		t.Fatalf("session block %q: %v", m[1], err)
	}
	return s.Token
}

func (f *termFixture) dialWith(host, origin string, protocols ...string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/ws", &websocket.DialOptions{Host: host, HTTPHeader: hdr, Subprotocols: protocols})
}

// The console of a page framed by another site (another registrable domain), with no cookie jar anywhere: the grant posted by the platform
// page is answered with the console page holding it, the page opens the socket with the grant as a subprotocol, only the first subprotocol
// comes back, no Set-Cookie ever goes out, no log line holds the grant, and the CSP has no 'unsafe-inline' for scripts.
func TestConsoleWorksCookielessInACrossSiteFrame(t *testing.T) {
	sink := &memSink{}
	f := newTermFixture(t, func(c *TerminalConfig) {
		c.Embed = matcher(t, []string{"https://catalog.example.org"}, nil)
		c.Log = logr.New(sink)
	})
	f.h.WithAccessLog(logr.New(sink), 0)
	raw := rehost(t, f.cookie, termHost)
	req, _ := http.NewRequest("POST", f.srv.URL+AuthPath, strings.NewReader(url.Values{"grant": {raw}}.Encode()))
	req.Host = termHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", platformOrigin)                         // the platform page that submits the form
	req.Header.Set("Referer", "https://catalog.example.org/tasks/1") // what the browser sends for the frame
	resp, err := http.DefaultClient.Do(req)                          // a client with no cookie jar
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the page: %d %s", resp.StatusCode, body.String())
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 || resp.Header.Get("Location") != "" {
		t.Fatalf("the console answer sets no cookie and does not redirect: %v", resp.Header)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://catalog.example.org") {
		t.Errorf("the allowed ancestor is not named: %s", csp)
	}
	for _, d := range strings.Split(csp, ";") {
		if strings.Contains(d, "script-src") && (strings.Contains(d, "unsafe") || strings.Contains(d, "nonce") || strings.Contains(d, "http")) {
			t.Errorf("script-src is not strict: %q", d)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: %v", resp.Header)
	}
	if strings.Contains(body.String(), "onclick=") || strings.Contains(body.String(), "<script>") {
		t.Errorf("an inline script or handler in the page")
	}
	token := consoleToken(t, body.String())
	if token != raw {
		t.Error("the page holds the grant that was posted, unchanged")
	}

	// the socket: from the framing site's origin, token as the second subprotocol
	c, hresp, err := f.dialWith(termHost, "https://catalog.example.org", ConsoleSubprotocol, token)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	if got := c.Subprotocol(); got != ConsoleSubprotocol || strings.Contains(hresp.Header.Get("Sec-WebSocket-Protocol"), token) {
		t.Fatalf("only the first subprotocol comes back: %q / %q", got, hresp.Header.Get("Sec-WebSocket-Protocol"))
	}
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	if m := readText(t, c); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// the console's own origin works too (a top-level page); another site's does not, token or not
	if c2, _, err := f.dialWith(termHost, "http://"+termHost, ConsoleSubprotocol, token); err != nil {
		t.Errorf("the host's own origin: %v", err)
	} else {
		_ = c2.CloseNow()
	}
	for _, origin := range []string{"https://evil.example.net", "https://catalog.example.org.evil.net", ""} {
		if c3, _, err := f.dialWith(termHost, origin, ConsoleSubprotocol, token); err == nil {
			_ = c3.CloseNow()
			t.Errorf("origin %q opened the console", origin)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if logs := sink.all(); strings.Contains(logs, token) {
		t.Errorf("a log line holds the grant:\n%s", logs)
	}
}

// What the grant is worth on the socket: one host, one signature, one kind, a limited time, and the exact subprotocols.
func TestConsoleGrantIsCheckedOnTheSocket(t *testing.T) {
	f := newTermFixture(t, nil)
	rec := f.get("/", nil)
	token := consoleToken(t, rec.Body.String())
	dial := func(host string, protocols ...string) error {
		c, _, err := f.dialWith(host, "http://"+host, protocols...)
		if err == nil {
			_ = c.CloseNow()
		}
		return err
	}
	if err := dial(termHost, ConsoleSubprotocol, token); err != nil {
		t.Fatalf("the grant of this host: %v", err)
	}
	// a grant for host A on host B (both consoles of the same group)
	if err := dial(termHostFor("lab1", "plain"), ConsoleSubprotocol, token); err == nil {
		t.Error("a grant of one console host opened another")
	}
	sc := sessionOf(t, f.cookie)
	sc.Host, sc.Kind = termHost, grant.KindConsole
	sc.Lab, sc.Dev = termLabDevice(termHost)
	sc.Audience = []string{grant.AudienceProxy, grant.AudienceDeviceAccess}
	// expired
	expired := sc
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	if err := dial(termHost, ConsoleSubprotocol, signCookie(t, expired)); err == nil {
		t.Error("an expired grant opened the console")
	}
	// the grant of a web session (kind web) is no console grant
	web := sc
	web.Kind = grant.KindWeb
	if err := dial(termHost, ConsoleSubprotocol, signCookie(t, web)); err == nil {
		t.Error("a web grant opened the console")
	}
	// a grant that is not for the proxy
	deviceOnly := sc
	deviceOnly.Audience = []string{grant.AudienceDeviceAccess}
	if err := dial(termHost, ConsoleSubprotocol, signCookie(t, deviceOnly)); err == nil {
		t.Error("a grant without the proxy audience opened the console")
	}
	// and a console grant is no web cookie
	web2 := "web-k10w." + termBaseDom
	req2, _ := http.NewRequest("GET", f.srv.URL+"/", nil)
	req2.Host = web2
	req2.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	if resp, err := http.DefaultClient.Do(req2); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Error("a console grant opened a web host as a cookie")
		}
	}
	// a grant signed with another key
	other := granttest.NewKey(t, "acme", "k1")
	if err := dial(termHost, ConsoleSubprotocol, other.Sign(t, claimsOf(sc, time.Now()))); err == nil {
		t.Error("a grant signed with another key opened the console")
	}
	// the subprotocols: wrong name, missing grant, grant first, extra entries, none
	for name, protos := range map[string][]string{
		"wrong name":     {"cib.console.v2", token},
		"no grant":       {ConsoleSubprotocol},
		"grant first":    {token, ConsoleSubprotocol},
		"an extra entry": {ConsoleSubprotocol, token, "x"},
		"none":           nil,
	} {
		if err := dial(termHost, protos...); err == nil {
			t.Errorf("%s: the console opened", name)
		}
	}
	// no cookie is looked at on the console host, and no cookie path serves it
	for _, path := range []string{"/", "/ws"} {
		req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
		req.Host = termHost
		req.AddCookie(&http.Cookie{Name: "challenge", Value: rehostWeb(t, f.cookie, termHost)})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a web cookie on the console host: %d", path, resp.StatusCode)
		}
	}
}

// A reload of the console page (a GET holds no grant) is "session over".
func TestConsolePageWithoutLinkIsSessionOver(t *testing.T) {
	f := newTermFixture(t, nil)
	f.cookie = "" // nothing makes a link
	rec := f.get("/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Сесію завершено") || strings.Contains(rec.Body.String(), `id="session"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// The page script leaves the posted path for the root (a reload is a GET, not a second post) and offers the grant as the second subprotocol.
func TestConsoleScriptDropsTheLinkAndUsesSubprotocols(t *testing.T) {
	js, err := terminalFS.ReadFile("terminalassets/terminal.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{"history.replaceState", "'cib.console.v1'", "SESSION.token", "document.getElementById('session')"} {
		if !strings.Contains(src, want) {
			t.Errorf("terminal.js lacks %s", want)
		}
	}
	if strings.Contains(src, "document.cookie") || strings.Contains(src, "localStorage") || strings.Contains(src, "sessionStorage") {
		t.Error("the console script stores the token somewhere")
	}
	if i, j := strings.Index(src, "history.replaceState"), strings.Index(src, "new WebSocket("); i < 0 || j < 0 || i > j {
		t.Error("the link must be removed from the address before the socket opens")
	}
}

func rehostWeb(t *testing.T, token, host string) string {
	t.Helper()
	sc := sessionOf(t, token)
	sc.Host = host
	return signCookie(t, sc)
}

// A console ends when the grant it was opened with ends: the live check closes it at exp, whatever the lifetime limit says, and a grant that
// has ended opens nothing.
func TestConsoleEndsAtTheGrantExp(t *testing.T) {
	f := newTermFixture(t, nil)
	sc := sessionOf(t, f.cookie)
	sc.Host, sc.Kind = termHost, grant.KindConsole
	sc.Lab, sc.Dev = termLabDevice(termHost)
	sc.Audience = []string{grant.AudienceProxy, grant.AudienceDeviceAccess}
	sc.ExpiresAt = jwt.NewNumericDate(time.Now().Add(1500 * time.Millisecond))
	c, _, err := f.dialWith(termHost, "http://"+termHost, ConsoleSubprotocol, signCookie(t, sc))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 16)
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	if m := readText(t, c); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	}
	if n := f.h.CheckLive(); n != 0 {
		t.Fatalf("closed %d before the grant ended", n)
	}
	time.Sleep(1700 * time.Millisecond)
	if n := f.h.CheckLive(); n != 1 {
		t.Fatalf("closed %d at the end of the grant, want 1", n)
	}
	rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reason := ""
	for {
		typ, msg, err := c.Read(rctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageBinary {
			if ft, p, _ := wire.Parse(msg); ft == wire.TypeClose {
				reason = string(p)
			}
		}
	}
	// the close frame may be lost with the connection that the cancelled context closes; when it arrives it says why
	if reason != "" && reason != wire.ReasonLifetime {
		t.Fatalf("closed with %q, want %q", reason, wire.ReasonLifetime)
	}
	// and now that grant opens nothing (past exp and the clock leeway)
	sc.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	if c2, _, err := f.dialWith(termHost, "http://"+termHost, ConsoleSubprotocol, signCookie(t, sc)); err == nil {
		_ = c2.CloseNow()
		t.Fatal("an ended grant opened a console")
	}
}
