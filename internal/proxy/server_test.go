package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, cfg *L7Config) string {
	t.Helper()
	srv := NewHTTPServer(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}), nil)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

func shortLimits() *L7Config {
	return &L7Config{ReadHeaderTimeout: 300 * time.Millisecond, ReadTimeout: time.Second, IdleTimeout: 400 * time.Millisecond, MaxHeaderBytes: 1024}
}

// readsUntilClosed says whether the server closes the connection within the time.
func closedWithin(conn net.Conn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.ReadAll(conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return true
}

// The slowloris of the audit: headers that never finish do not hold the connection.
func TestSlowHeadersAreCut(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-A: ") // one incomplete header line, then silence
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("a connection that never finishes its headers must be closed")
	}
}

// A body that stalls is cut by the read timeout.
func TestSlowBodyIsCut(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\nabc")
	if !closedWithin(conn, 4*time.Second) {
		t.Fatal("a stalled body must be cut by the read timeout")
	}
}

// A kept-alive connection with no next request is closed after the idle timeout; a normal request still works.
func TestIdleConnectionIsClosedAndNormalRequestsWork(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("a normal request: %v %v", resp, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("an idle kept-alive connection must be closed")
	}
}

// Headers over the limit are refused, not buffered.
func TestHugeHeadersAreRefused(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-Big: %s\r\n\r\n", strings.Repeat("a", 8192))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %v err = %v, want 431", resp, err)
	}
}

func TestServerLimitsComeFromTheConfig(t *testing.T) {
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("TLS_CERT_PATH", "/x")
	t.Setenv("TLS_KEY_PATH", "/y")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	cfg, err := LoadL7Config()
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTPServer(cfg, http.NotFoundHandler(), nil)
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 5*time.Minute || srv.IdleTimeout != 2*time.Minute || srv.MaxHeaderBytes != 65536 || srv.WriteTimeout != 0 {
		t.Fatalf("limits: %v %v %v %d %v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout, srv.MaxHeaderBytes, srv.WriteTimeout)
	}
	if cfg.AccessTokenMaxTTL != 60*time.Second {
		t.Fatalf("the access token limit is 60s: %v", cfg.AccessTokenMaxTTL)
	}
	t.Setenv("READ_HEADER_TIMEOUT", "0s")
	if _, err := LoadL7Config(); err == nil {
		t.Fatal("a zero timeout must be refused")
	}
}

func TestWGLimitsDefaults(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	cfg, err := LoadWGConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxEntries != 100000 || cfg.PartialTTL != 15*time.Second || cfg.MissRate != 2000 || cfg.MissBurst != 4000 ||
		cfg.RoamInterval != 5*time.Second || cfg.GlobalHandshakeRate != 2000 || cfg.SessionRate != 15000 || cfg.Readers != 4 {
		t.Fatalf("%+v", cfg)
	}
	t.Setenv("DEMUX_MAX_ENTRIES", "0")
	if _, err := LoadWGConfig(); err == nil {
		t.Fatal("a zero cap must be refused")
	}
}

// R-17: the server holds at most so many connections at once; one that closes frees its place.
func TestLimitListenerCapsConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := LimitListener(ln, 2)
	defer l.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	for i := 0; i < 3; i++ {
		dial()
	}
	var held []net.Conn
	for i := 0; i < 2; i++ {
		select {
		case c := <-accepted:
			held = append(held, c)
		case <-time.After(2 * time.Second):
			t.Fatal("two connections fit")
		}
	}
	select {
	case <-accepted:
		t.Fatal("a third connection must wait")
	case <-time.After(200 * time.Millisecond):
	}
	_ = held[0].Close() // frees a place
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting connection is accepted once one closes")
	}
}

func TestL7LimitsDefaults(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "/c")
	t.Setenv("TLS_KEY_PATH", "/k")
	t.Setenv("BASE_DOMAIN", "x.y")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 40))
	cfg, err := LoadL7Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConnections != 2500 || cfg.LivePerClient != 200 || cfg.LivePerGroup != 1000 || cfg.LiveTotal != 2500 || cfg.AuthRate != 5 || cfg.AuthBurst != 20 {
		t.Fatalf("%+v", cfg)
	}
}

func proxyHeaderV1(src string) string { return "PROXY TCP4 " + src + " 10.0.0.1 5555 443\r\n" }

// A listed peer's PROXY header gives the connection's address, and the header is optional for it; the header of any other peer is
// consumed and ignored (nobody can claim an address); an empty list changes nothing.
func TestProxyProtocolListener(t *testing.T) {
	remote := func(trusted []string, send string) string {
		t.Helper()
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ln, err := ProxyProtocolListener(raw, trusted, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		got := make(chan string, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				got <- "accept: " + err.Error()
				return
			}
			defer c.Close()
			buf := make([]byte, 5)
			if _, err := io.ReadFull(c, buf); err != nil {
				got <- "read: " + err.Error()
				return
			}
			host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			got <- host + " " + string(buf)
		}()
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = io.WriteString(c, send+"hello")
		select {
		case v := <-got:
			return v
		case <-time.After(3 * time.Second):
			t.Fatal("no connection")
			return ""
		}
	}
	if got := remote([]string{"127.0.0.0/8"}, proxyHeaderV1("198.51.100.7")); got != "198.51.100.7 hello" {
		t.Errorf("trusted peer with a header: %q", got)
	}
	if got := remote([]string{"127.0.0.1"}, ""); got != "127.0.0.1 hello" {
		t.Errorf("trusted peer without a header (optional): %q", got)
	}
	if got := remote([]string{"10.0.0.0/8"}, proxyHeaderV1("198.51.100.7")); got != "127.0.0.1 hello" {
		t.Errorf("an untrusted peer's header must be consumed and ignored: %q", got)
	}
	if got := remote(nil, ""); got != "127.0.0.1 hello" {
		t.Errorf("feature off: %q", got)
	}
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	defer raw.Close()
	if ln, err := ProxyProtocolListener(raw, nil, time.Second); err != nil || ln != raw {
		t.Errorf("an empty list must return the listener unchanged: %v", err)
	}
	if _, err := ProxyProtocolListener(raw, []string{"not-an-ip"}, time.Second); err == nil {
		t.Error("a bad entry must fail")
	}
}
