package egressproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/dns/dnsmessage"
)

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM":       "example.com",
		"example.com.":      "example.com",
		"example.com:8080":  "example.com",
		"a-b_c.example.org": "a-b_c.example.org",
	} {
		got, reason := NormalizeName(in)
		if got != want || reason != "" {
			t.Errorf("%q -> %q (%s), want %q", in, got, reason, want)
		}
	}
	for in, want := range map[string]string{
		"":                    ReasonNoName,
		"1.2.3.4":             ReasonIPName,
		"1.2.3.4:443":         ReasonIPName,
		"[::1]":               ReasonIPName,
		"[::1]:443":           ReasonIPName,
		"::1":                 ReasonIPName,
		"2130706433":          ReasonIPName,
		"0x7f000001":          ReasonIPName,
		"127.1":               ReasonIPName,
		"0x7f.1":              ReasonIPName,
		"exa mple.com":        ReasonBadName,
		"-a.com":              ReasonBadName,
		"a..com":              ReasonBadName,
		"ünicode.example.com": ReasonBadName,
	} {
		if got, reason := NormalizeName(in); got != "" || reason != want {
			t.Errorf("%q -> %q (%s), want refused as %s", in, got, reason, want)
		}
	}
}

func TestDenylist(t *testing.T) {
	d := NewDenylist(DefaultTunnelSuffixes)
	for _, n := range []string{"ngrok.io", "abc.ngrok-free.app", "x.y.trycloudflare.com", "controlplane.tailscale.com", "my.zerotier.com", "a.loca.lt"} {
		if !d.Denied(n) {
			t.Errorf("%s must be refused", n)
		}
	}
	for _, n := range []string{"github.com", "notngrok.io", "ngrok.io.example.com", "pypi.org", "cloudflare.com"} {
		if d.Denied(n) {
			t.Errorf("%s must be open", n)
		}
	}
}

// hello builds a ClientHello record by hand, with the given extensions.
func hello(exts ...[]byte) []byte {
	var body []byte
	body = append(body, 3, 3)                // version
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0)                   // session id
	body = append(body, 0, 2, 0x13, 0x01)    // one cipher suite
	body = append(body, 1, 0)                // compression
	var e []byte
	for _, x := range exts {
		e = append(e, x...)
	}
	body = append(body, byte(len(e)>>8), byte(len(e)))
	body = append(body, e...)
	hs := append([]byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	return append([]byte{22, 3, 1, byte(len(hs) >> 8), byte(len(hs))}, hs...)
}

func ext(typ int, data []byte) []byte {
	return append([]byte{byte(typ >> 8), byte(typ), byte(len(data) >> 8), byte(len(data))}, data...)
}

func sniExt(name string) []byte {
	entry := append([]byte{0, byte(len(name) >> 8), byte(len(name))}, name...)
	list := append([]byte{byte(len(entry) >> 8), byte(len(entry))}, entry...)
	return ext(0, list)
}

func TestReadClientHello(t *testing.T) {
	h, err := ReadClientHello(bytes.NewReader(hello(sniExt("Example.org"))))
	if err != nil || h.ServerName != "Example.org" || h.ECH {
		t.Fatalf("%+v %v", h, err)
	}
	h, err = ReadClientHello(bytes.NewReader(hello(sniExt("example.org"), ext(extECH, []byte{0, 1, 2}))))
	if err != nil || !h.ECH {
		t.Fatalf("ECH not seen: %+v %v", h, err)
	}
	h, err = ReadClientHello(bytes.NewReader(hello()))
	if err != nil || h.ServerName != "" {
		t.Fatalf("no extensions: %+v %v", h, err)
	}
	// the ClientHello split over two TLS records
	full := hello(sniExt("split.example.org"))
	first := append([]byte{22, 3, 1, 0, 20}, full[5:25]...)
	second := append([]byte{22, 3, 1, byte((len(full) - 25) >> 8), byte(len(full) - 25)}, full[25:]...)
	h, err = ReadClientHello(bytes.NewReader(append(first, second...)))
	if err != nil || h.ServerName != "split.example.org" || !bytes.Equal(h.Raw, append(first, second...)) {
		t.Fatalf("split hello: %+v %v", h, err)
	}
	for name, in := range map[string][]byte{"http": []byte("GET / HTTP/1.1\r\n\r\n"), "app data": {23, 3, 3, 0, 1, 0}, "empty": nil} {
		if _, err := ReadClientHello(bytes.NewReader(in)); err == nil {
			t.Errorf("%s accepted as a ClientHello", name)
		}
	}
}

type fixture struct {
	p        *Proxy
	httpAddr string
	tlsAddr  string
	upHTTP   *httptest.Server
	upTLS    *httptest.Server
	stats    *Collector
}

func newFixture(t *testing.T, configure ...func(*Proxy)) *fixture {
	t.Helper()
	f := &fixture{}
	f.upHTTP = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "hello from "+r.Host) }))
	f.upTLS = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "secure hello") }))
	t.Cleanup(f.upHTTP.Close)
	t.Cleanup(f.upTLS.Close)
	labs := NewLabMap()
	_ = labs.Set("127.0.0.0/8", "lab-a")
	r := &Resolver{cache: map[string]cacheEntry{}, TTL: time.Hour, NegativeTTL: time.Hour, Now: time.Now}
	exp := time.Now().Add(time.Hour)
	for _, n := range []string{"allowed.test", "ngrok.io", "busy.test"} {
		r.cache[n] = cacheEntry{ips: []netip.Addr{netip.MustParseAddr("203.0.113.9")}, expires: exp}
	}
	f.stats = NewCollector("gw-test")
	f.p = &Proxy{Labs: labs, Resolver: r, Deny: NewDenylist(DefaultTunnelSuffixes), Stats: f.stats, Log: logr.Discard(), HostNewPerSecond: 5,
		Dial: func(_ context.Context, addr string) (net.Conn, error) {
			// the address the proxy looked up (203.0.113.9) is replaced by the test server of the port
			if strings.HasSuffix(addr, ":443") {
				return net.Dial("tcp", strings.TrimPrefix(f.upTLS.URL, "https://"))
			}
			return net.Dial("tcp", strings.TrimPrefix(f.upHTTP.URL, "http://"))
		}}
	for _, c := range configure {
		c(f.p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hl, _ := net.Listen("tcp4", "127.0.0.1:0")
	tl, _ := net.Listen("tcp4", "127.0.0.1:0")
	go func() { _ = f.p.Serve(ctx, hl, false) }()
	go func() { _ = f.p.Serve(ctx, tl, true) }()
	f.httpAddr, f.tlsAddr = hl.Addr().String(), tl.Addr().String()
	return f
}

func httpGet(addr, host string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	b, err := io.ReadAll(c)
	return string(b), err
}

func TestProxyForwardsByNameAndIgnoresTheAddress(t *testing.T) {
	f := newFixture(t)
	out, err := httpGet(f.httpAddr, "allowed.test")
	if err != nil || !strings.Contains(out, "200 OK") || !strings.Contains(out, "hello from allowed.test") {
		t.Fatalf("HTTP through the proxy: %q %v", out, err)
	}
	// HTTPS: the client says allowed.test; the address it dialled (the proxy's) is of no importance
	conn, err := tls.Dial("tcp", f.tlsAddr, &tls.Config{ServerName: "allowed.test", InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS through the proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(b), "secure hello") {
		t.Fatalf("TLS body: %q", b)
	}
	time.Sleep(100 * time.Millisecond)
	m := f.stats.Since(time.Time{})
	f.stats.mu.Lock()
	row := f.stats.rows[rowKey{"lab-a", "allowed.test"}]
	f.stats.mu.Unlock()
	if row == nil || row.Conns != 2 || row.Up == 0 || row.Down == 0 {
		t.Fatalf("summary row: %+v (finished minutes %d)", row, len(m.Minutes))
	}
}

func TestProxyRefuses(t *testing.T) {
	f := newFixture(t)
	dial := func() (net.Conn, error) { return net.DialTimeout("tcp", f.tlsAddr, time.Second) }
	closedAfter := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := io.ReadAll(c)
		var ne net.Error
		return err == nil || !errors.As(err, &ne) || !ne.Timeout() // closed, or reset by the proxy; not still open
	}
	// no server name (a client that connects to an address)
	if _, err := tls.Dial("tcp", f.tlsAddr, &tls.Config{ServerName: "", InsecureSkipVerify: true}); err == nil {
		t.Error("a TLS client with no server name got through")
	}
	// ECH
	c, _ := dial()
	_, _ = c.Write(hello(sniExt("allowed.test"), ext(extECH, []byte{1, 2, 3})))
	if !closedAfter(c) {
		t.Error("a ClientHello with ECH was not closed")
	}
	c.Close()
	// an IP as the name, in a ClientHello
	c, _ = dial()
	_, _ = c.Write(hello(sniExt("203.0.113.9")))
	if !closedAfter(c) {
		t.Error("an IP as server name was not closed")
	}
	c.Close()
	// a tunnel service
	if _, err := tls.Dial("tcp", f.tlsAddr, &tls.Config{ServerName: "ngrok.io", InsecureSkipVerify: true}); err == nil {
		t.Error("ngrok.io got through")
	}
	// plain HTTP: bare IP in Host, a tunnel, no Host
	for host, want := range map[string]string{"203.0.113.9": "403", "ngrok.io": "403", "": "403"} {
		out, _ := httpGet(f.httpAddr, host)
		if !strings.Contains(out, want) {
			t.Errorf("Host %q: %q", host, out)
		}
	}
	// not TLS on 443, not HTTP on 80
	c, _ = dial()
	_, _ = io.WriteString(c, "SSH-2.0-OpenSSH_9.0\r\n")
	if !closedAfter(c) {
		t.Error("SSH on 443 was not closed")
	}
	c.Close()
	c, _ = net.Dial("tcp", f.httpAddr)
	_, _ = io.WriteString(c, "SSH-2.0-OpenSSH_9.0\r\n\r\n")
	if !closedAfter(c) {
		t.Error("SSH on 80 was not closed")
	}
	c.Close()
}

func TestProxyLimits(t *testing.T) {
	f := newFixture(t, func(p *Proxy) { p.HostNewPerSecond = 3 })
	ok, refused := 0, 0
	for i := 0; i < 12; i++ {
		out, _ := httpGet(f.httpAddr, "busy.test")
		if strings.Contains(out, "200 OK") {
			ok++
		} else {
			refused++
		}
	}
	if ok == 0 || refused == 0 || ok > 5 {
		t.Fatalf("per-host new connection rate: %d served, %d refused", ok, refused)
	}
	g := newFixture(t, func(p *Proxy) { p.HostNewPerSecond = 0; p.LabMaxConns = 1 })
	hold, _ := net.Dial("tcp", g.httpAddr) // holds the only slot while it sends nothing
	time.Sleep(100 * time.Millisecond)
	out, _ := httpGet(g.httpAddr, "allowed.test")
	hold.Close()
	if strings.Contains(out, "200 OK") {
		t.Fatalf("a lab over its connection cap was served: %q", out)
	}
}

func TestProxyEndsOldConnections(t *testing.T) {
	f := newFixture(t, func(p *Proxy) { p.MaxConnAge = 200 * time.Millisecond })
	c, _ := net.Dial("tcp", f.httpAddr)
	defer c.Close()
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: allowed.test\r\n\r\n") // keep-alive: the connection stays
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err := io.ReadAll(c)
	if err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("the connection outlived its age: %v after %v", err, time.Since(start))
	}
}

func TestDNSAnswersAndRefuses(t *testing.T) {
	labs := NewLabMap()
	_ = labs.Set("10.9.1.0/24", "lab-a")
	r := &Resolver{cache: map[string]cacheEntry{}, TTL: time.Hour, NegativeTTL: time.Hour, Now: time.Now}
	r.cache["allowed.test"] = cacheEntry{ips: []netip.Addr{netip.MustParseAddr("203.0.113.9")}, expires: time.Now().Add(time.Hour)}
	d := &DNS{Labs: labs, Resolver: r, Deny: NewDenylist(DefaultTunnelSuffixes), Stats: NewCollector("g"), Log: logr.Discard(), QueriesPerSecond: 100}
	ask := func(from string, typ dnsmessage.Type, name string) dnsmessage.Message {
		n, _ := dnsmessage.NewName(name + ".")
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: n, Type: typ, Class: dnsmessage.ClassINET}}}
		b, _ := q.Pack()
		out := d.Handle(context.Background(), netip.MustParseAddr(from), b)
		var m dnsmessage.Message
		if out == nil {
			return dnsmessage.Message{Header: dnsmessage.Header{ID: 0xffff}}
		}
		if err := m.Unpack(out); err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := ask("10.9.1.5", dnsmessage.TypeA, "allowed.test")
	if len(a.Answers) != 1 || a.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{203, 0, 113, 9} {
		t.Fatalf("A: %+v", a)
	}
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeAAAA, dnsmessage.TypeHTTPS, dnsmessage.TypeSVCB, dnsmessage.TypeTXT} {
		if m := ask("10.9.1.5", typ, "allowed.test"); len(m.Answers) != 0 || m.RCode != dnsmessage.RCodeSuccess {
			t.Errorf("type %v must be an empty answer: %+v", typ, m)
		}
	}
	if m := ask("10.9.1.5", dnsmessage.TypeA, "1.2.3.4"); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("an IP as a name: %+v", m)
	}
	if m := ask("10.9.1.5", dnsmessage.TypeA, "abc.ngrok.io"); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("a tunnel service: %+v", m)
	}
	if m := ask("192.0.2.1", dnsmessage.TypeA, "allowed.test"); m.ID != 0xffff {
		t.Errorf("an address that belongs to no lab was answered")
	}
	d.limits, d.QueriesPerSecond = nil, 5
	answered := 0
	for i := 0; i < 30; i++ {
		if m := ask("10.9.1.5", dnsmessage.TypeA, "allowed.test"); m.ID != 0xffff {
			answered++
		}
	}
	if answered > 8 {
		t.Errorf("the per-lab query rate was not applied: %d of 30 answered", answered)
	}
}

func TestCollectorMinutes(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 30, 0, time.UTC)
	c := NewCollector("g")
	c.Now = func() time.Time { return now }
	c.Start("lab-a", "x.test")
	c.End("lab-a", "x.test", 10, 20)
	c.Denied("lab-a", "", ReasonECH)
	c.Cap("lab-a", "lab-rate")
	if got := c.Since(time.Time{}); len(got.Minutes) != 0 {
		t.Fatalf("the minute is not finished: %+v", got)
	}
	now = now.Add(time.Minute)
	got := c.Since(time.Time{})
	if len(got.Minutes) != 1 || got.Gateway != "g" {
		t.Fatalf("%+v", got)
	}
	m := got.Minutes[0]
	if m.Rows[0].Conns != 1 || m.Rows[0].Up != 10 || m.Rows[0].Down != 20 || m.Caps[0].Reason != "lab-rate" || m.Denials[0].Reason != ReasonECH {
		t.Fatalf("%+v", m)
	}
	if again := c.Since(m.Start); len(again.Minutes) != 0 {
		t.Fatalf("a minute already pulled came again: %+v", again)
	}
}

type countingReader struct {
	read int
	fill func(p []byte)
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.fill(p)
	c.read += len(p)
	return len(p), nil
}

// The proxy never buffers more than a head (HTTP) or 64 kB (a ClientHello), whatever the client sends: the read itself is bounded.
func TestReadsAreBounded(t *testing.T) {
	garbage := &countingReader{fill: func(p []byte) {
		for i := range p {
			p[i] = 'a'
		}
	}}
	if _, _, err := readHTTPHead(garbage); err == nil {
		t.Fatal("an endless line was accepted as a head")
	}
	if garbage.read > maxHTTPHead+8192 {
		t.Errorf("the HTTP head read took %d bytes, the bound is %d", garbage.read, maxHTTPHead)
	}
	// a request line that never ends with CRLF CRLF, in many small lines
	lines := &countingReader{fill: func(p []byte) {
		for i := range p {
			p[i] = "X-A: b\r\n"[i%8]
		}
	}}
	if _, _, err := readHTTPHead(io.MultiReader(strings.NewReader("GET / HTTP/1.1\r\n"), lines)); err == nil {
		t.Fatal("an endless head was accepted")
	}
	if lines.read > maxHTTPHead+8192 {
		t.Errorf("the endless head read took %d bytes", lines.read)
	}
	// a ClientHello cut into 1-byte records, without end
	rec := []byte{22, 3, 1, 0, 1, 1}
	tiny := &countingReader{fill: func(p []byte) {
		for i := range p {
			p[i] = rec[i%len(rec)]
		}
	}}
	if _, err := ReadClientHello(tiny); err == nil {
		t.Fatal("an endless ClientHello was accepted")
	}
	if tiny.read > maxHelloBytes+8192 {
		t.Errorf("the ClientHello read took %d bytes, the bound is %d", tiny.read, maxHelloBytes)
	}
}

// The checks of the HTTP head are the checks a server would apply: the name the proxy judges is the name the server serves, and every request
// of a connection is one that was checked.
func TestHTTPHeadChecks(t *testing.T) {
	good := map[string]string{
		"GET / HTTP/1.1\r\nHost: a.test\r\n\r\n":                          "a.test",
		"GET http://a.test/x HTTP/1.1\r\nHost: a.test:80\r\n\r\n":         "a.test:80",
		"GET http://A.TEST/x HTTP/1.1\r\nHost: a.test\r\n\r\n":            "a.test",
		"GET http://a.test/x HTTP/1.0\r\n\r\n":                            "a.test",
		"POST / HTTP/1.1\r\nHost: a.test\r\nContent-Length: 3\r\n\r\nabc": "a.test",
		"OPTIONS * HTTP/1.1\r\nHost: a.test\r\n\r\n":                      "a.test",
	}
	for in, want := range good {
		_, host, err := readHTTPHead(strings.NewReader(in))
		if err != nil || host != want {
			t.Errorf("%q -> %q %v, want %q", in, host, err, want)
		}
	}
	bad := map[string]string{
		"two Hosts":              "GET / HTTP/1.1\r\nHost: a.test\r\nHost: b.test\r\n\r\n",
		"two Hosts, same":        "GET / HTTP/1.1\r\nHost: a.test\r\nhost: a.test\r\n\r\n",
		"target and Host differ": "GET http://b.test/ HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"target with userinfo":   "GET http://u@a.test/ HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"folded header":          "GET / HTTP/1.1\r\nHost: a.test\r\nX-A: b\r\n c\r\n\r\n",
		"folded Host":            "GET / HTTP/1.1\r\nX: y\r\n Host: b.test\r\nHost: a.test\r\n\r\n",
		"space before colon":     "GET / HTTP/1.1\r\nHost : a.test\r\n\r\n",
		"CONNECT":                "CONNECT a.test:443 HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"bare LF":                "GET / HTTP/1.1\nHost: a.test\n\n",
		"bad target":             "GET a.test HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"not HTTP":               "SSH-2.0-OpenSSH_9.0\r\n\r\n",
		"empty line in the head": "GET / HTTP/1.1\r\n\r\nHost: a.test\r\n\r\n",
	}
	for name, in := range bad {
		if _, host, err := readHTTPHead(strings.NewReader(in)); err == nil && name != "empty line in the head" {
			t.Errorf("%s was accepted (host %q)", name, host)
		}
	}
	// "empty line in the head" is the end of the head: the second "request" is body, and the first has no Host
	if _, host, _ := readHTTPHead(strings.NewReader(bad["empty line in the head"])); host != "" {
		t.Errorf("the Host after the end of the head was taken: %q", host)
	}
	// forwarded as one request per connection
	fwd, _, _ := readHTTPHead(strings.NewReader("GET / HTTP/1.1\r\nHost: a.test\r\nConnection: keep-alive\r\nUpgrade: websocket\r\nProxy-Connection: keep-alive\r\nKeep-Alive: 5\r\nX-Keep: 1\r\n\r\n"))
	got := string(fwd)
	for _, gone := range []string{"keep-alive", "Upgrade", "websocket", "Proxy-Connection", "Keep-Alive"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q must not be forwarded:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "Connection: close\r\n\r\n") || !strings.Contains(got, "X-Keep: 1") || !strings.Contains(got, "Host: a.test") {
		t.Errorf("the forwarded head:\n%s", got)
	}
}

// Through the proxy: the server gets Connection: close, and a second request on the same connection is not served.
func TestProxyServesOneRequestPerConnection(t *testing.T) {
	f := newFixture(t)
	c, err := net.Dial("tcp", f.httpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: allowed.test\r\nConnection: keep-alive\r\n\r\nGET / HTTP/1.1\r\nHost: ngrok.io\r\n\r\n")
	b, _ := io.ReadAll(c)
	if n := strings.Count(string(b), "HTTP/1.1 200"); n != 1 {
		t.Fatalf("%d responses on one connection:\n%s", n, b)
	}
	if strings.Contains(string(b), "ngrok") {
		t.Errorf("the second request was served:\n%s", b)
	}
}
