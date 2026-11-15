package l7

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

const (
	tunnelHostLabel = "bot-k7xg"
	tunnelHost      = tunnelHostLabel + ".challenges.example.com"
)

// tunnelRig is a proxy in front of a fake device (a TCP echo server), with a group, a running lab and the gRPC-kind Service of the device
// in a fake cluster. There is no LabGroupClient and no access policy at all: the driven device is not a client of the group.
type tunnelRig struct {
	t       *testing.T
	h       *Handler
	srv     *httptest.Server
	cluster client.Client
	priv    ed25519.PrivateKey
	keyOK   atomic.Bool
	echo    net.Listener
	dials   atomic.Int32
	svc     *corev1.Service
	lab     *lab.Lab
}

func newTunnelRig(t *testing.T) *tunnelRig {
	t.Helper()
	rig := &tunnelRig{t: t}
	rig.keyOK.Store(true)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	rig.priv = priv

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rig.echo = echo
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	rig.lab = &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}
	rig.svc = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: tunnelHostLabel, Namespace: "legacy",
		Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "bot", names.LabelAccessKind: "grpc"}},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Name: "grpc", Port: 9099, TargetPort: intstr.FromInt32(9099), Protocol: corev1.ProtocolTCP}}}}
	rig.cluster = fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, rig.lab, rig.svc).Build()

	keys := func(tenant, kid string) (ed25519.PublicKey, bool) {
		if tenant == "acme" && kid == "k1" && rig.keyOK.Load() {
			return pub, true
		}
		return nil, false
	}
	rig.h = NewHandler(keys, testSecret, "challenges.example.com", "challenge", nil).
		WithAccessReader(&AccessReader{Reader: rig.cluster}).
		WithTunnelDial(func(ctx context.Context, addr string) (net.Conn, error) {
			rig.dials.Add(1)
			if addr != "bot-k7xg.legacy.svc.cluster.local:9099" {
				return nil, io.ErrClosedPipe
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", echo.Addr().String())
		})
	rig.srv = httptest.NewServer(rig.h)
	t.Cleanup(rig.srv.Close)
	return rig
}

// handoff is the 60 s link of the platform for the gRPC-kind host, use "tunnel".
func (r *tunnelRig) handoff(mutate func(*handoffClaims)) string {
	r.t.Helper()
	now := time.Now()
	c := baseHandoff(now)
	c.Host, c.Use = tunnelHost, tunnelUse
	c.Session = now.Add(24 * time.Hour).Unix()
	if mutate != nil {
		mutate(&c)
	}
	return signHandoff(r.t, r.priv, "k1", c)
}

func (r *tunnelRig) post(host, path string, body any, hdr http.Header) (int, []byte) {
	r.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", r.srv.URL+path, bytes.NewReader(raw))
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (r *tunnelRig) exchange(handoff string) (TunnelToken, int) {
	r.t.Helper()
	code, body := r.post(tunnelHost, "/_access/exchange", map[string]string{"handoff": handoff}, nil)
	var tok TunnelToken
	if code == 200 {
		if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
			r.t.Fatalf("exchange answer %q: %v", body, err)
		}
	}
	return tok, code
}

func (r *tunnelRig) dial(host, origin string, protocols ...string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+"/_tunnel", &websocket.DialOptions{Host: host, HTTPHeader: hdr, Subprotocols: protocols})
}

func (r *tunnelRig) open() (*websocket.Conn, TunnelToken) {
	r.t.Helper()
	tok, code := r.exchange(r.handoff(nil))
	if code != 200 {
		r.t.Fatalf("exchange: %d", code)
	}
	c, _, err := r.dial(tunnelHost, "", TunnelSubprotocol, tok.Token)
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	r.t.Cleanup(func() { _ = c.CloseNow() })
	return c, tok
}

func echoOnce(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	// the tunnel is a byte stream: the answer may come in other pieces than the message went
	var got []byte
	for len(got) < len(msg) {
		typ, part, err := c.Read(ctx)
		if err != nil || typ != websocket.MessageBinary {
			t.Fatalf("echo after %d of %d bytes: %v", len(got), len(msg), err)
		}
		got = append(got, part...)
	}
	if string(got) != msg {
		t.Fatalf("echo differs (%d bytes)", len(got))
	}
}

// The whole path: the platform's link is exchanged for a token, the token opens a tunnel to the device, bytes go both ways, only the first
// subprotocol comes back, and nothing about a lab client or a policy is asked.
func TestTunnelCarriesBytesWithoutAClient(t *testing.T) {
	rig := newTunnelRig(t)
	tok, code := rig.exchange(rig.handoff(nil))
	if code != 200 || !tok.ExpiresAt.After(time.Now()) || !tok.MaxUntil.After(tok.ExpiresAt) && !tok.MaxUntil.Equal(tok.ExpiresAt) {
		t.Fatalf("exchange: %d %+v", code, tok)
	}
	c, resp, err := rig.dial(tunnelHost, "", TunnelSubprotocol, tok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != TunnelSubprotocol || strings.Contains(resp.Header.Get("Sec-WebSocket-Protocol"), tok.Token) {
		t.Fatalf("only the first subprotocol comes back: %q", resp.Header.Get("Sec-WebSocket-Protocol"))
	}
	echoOnce(t, c, "hello")
	echoOnce(t, c, strings.Repeat("x", 200000)[:100000]) // a message bigger than the library's default read limit
	// closing the tunnel closes the device side
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func TestTunnelIsNotForBrowsers(t *testing.T) {
	rig := newTunnelRig(t)
	for _, path := range []string{"/_access/exchange", "/_access/refresh"} {
		code, _ := rig.post(tunnelHost, path, map[string]string{"handoff": rig.handoff(nil), "token": "x"}, http.Header{"Origin": {"https://evil.example.net"}})
		if code != http.StatusForbidden {
			t.Errorf("%s with an Origin header: %d", path, code)
		}
	}
	tok, _ := rig.exchange(rig.handoff(nil))
	if c, _, err := rig.dial(tunnelHost, "https://evil.example.net", TunnelSubprotocol, tok.Token); err == nil {
		_ = c.CloseNow()
		t.Error("a request with an Origin header opened a tunnel")
	}
	if c, _, err := rig.dial(tunnelHost, "http://"+tunnelHost, TunnelSubprotocol, tok.Token); err == nil {
		_ = c.CloseNow()
		t.Error("even the host's own origin opened a tunnel")
	}
}

// What the link is worth: this host, use tunnel, the tenant's key, 60 s; and the other channels do not take it.
func TestTunnelHandoffIsBoundToHostAndUse(t *testing.T) {
	rig := newTunnelRig(t)
	for name, mutate := range map[string]func(*handoffClaims){
		"another host":      func(c *handoffClaims) { c.Host = "bot-zzzzg.challenges.example.com" },
		"the first label":   func(c *handoffClaims) { c.Host = tunnelHostLabel },
		"no use":            func(c *handoffClaims) { c.Use = "" },
		"another use":       func(c *handoffClaims) { c.Use = "console" },
		"lives past 60 s":   func(c *handoffClaims) { c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(2 * time.Minute)) },
		"another tenant":    func(c *handoffClaims) { c.GroupID = "g-other" },
		"already past sess": func(c *handoffClaims) { c.Session = time.Now().Add(-time.Minute).Unix() },
	} {
		if _, code := rig.exchange(rig.handoff(mutate)); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	// garbage and an empty body
	if code, _ := rig.post(tunnelHost, "/_access/exchange", map[string]string{"handoff": "a.b.c"}, nil); code != http.StatusUnauthorized {
		t.Errorf("garbage: %d", code)
	}
	// a tunnel link opens no page and no session on the web and console hosts
	for _, host := range []string{"web-abc12w.challenges.example.com", "kali-k10t.challenges.example.com"} {
		link := rig.handoff(func(c *handoffClaims) { c.Host = host })
		req := httptest.NewRequest("GET", "https://"+host+AuthPath+"?t="+link, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		if rec.Code == http.StatusSeeOther || rec.Code == 200 || len(rec.Result().Cookies()) != 0 {
			t.Errorf("a tunnel link on %s: %d %v", host, rec.Code, rec.Result().Cookies())
		}
	}
	// and a web link starts no tunnel
	if _, code := rig.exchange(signHandoff(t, rig.priv, "k1", func() handoffClaims {
		c := baseHandoff(time.Now())
		c.Host = tunnelHost
		return c
	}())); code != http.StatusUnauthorized {
		t.Errorf("a web link on the exchange: %d", code)
	}
}

func TestTunnelTokenIsCheckedOnTheSocket(t *testing.T) {
	rig := newTunnelRig(t)
	tok, _ := rig.exchange(rig.handoff(nil))
	var claims jwtClaims
	_, _ = jwt.ParseWithClaims(tok.Token, &claims, func(*jwt.Token) (any, error) { return testSecret, nil })
	sign := func(c jwtClaims) string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(testSecret)
		return s
	}
	dial := func(host string, protos ...string) error {
		c, _, err := rig.dial(host, "", protos...)
		if err == nil {
			_ = c.CloseNow()
		}
		return err
	}
	if err := dial(tunnelHost, TunnelSubprotocol, tok.Token); err != nil {
		t.Fatalf("the token of this host: %v", err)
	}
	// a token of another host (the same group), expired, another use, another secret
	other := claims
	other.Host = "bot-zzzzg.challenges.example.com"
	expired := claims
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	console := claims
	console.Use = "console"
	web := claims
	web.Use = ""
	foreign, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("another-secret-another-secret-123456"))
	for name, token := range map[string]string{"another host": sign(other), "expired": sign(expired), "a console token": sign(console), "a web session token": sign(web), "another secret": foreign} {
		if err := dial(tunnelHost, TunnelSubprotocol, token); err == nil {
			t.Errorf("%s opened the tunnel", name)
		}
	}
	// a console token is not a tunnel token the other way round either
	if _, err := rig.h.verifySessionToken(tok.Token, tunnelHost, "console"); err == nil {
		t.Error("a tunnel token passed as a console token")
	}
	for name, protos := range map[string][]string{
		"wrong name": {"cib.tunnel.v2", tok.Token}, "no token": {TunnelSubprotocol}, "token first": {tok.Token, TunnelSubprotocol},
		"extra entry": {TunnelSubprotocol, tok.Token, "x"}, "none": nil, "the console protocol": {ConsoleSubprotocol, tok.Token},
	} {
		if err := dial(tunnelHost, protos...); err == nil {
			t.Errorf("%s: the tunnel opened", name)
		}
	}
	// the token of the host on the console path of the same name is nothing
	if code, _ := rig.post(tunnelHost, "/_access/refresh", map[string]string{"token": sign(expired)}, nil); code != http.StatusUnauthorized {
		t.Errorf("refresh with an expired token: %d", code)
	}
}

func TestTunnelRefreshNeverPassesMaxUntil(t *testing.T) {
	rig := newTunnelRig(t)
	rig.h.WithTunnelTokenTTL(time.Hour)
	tok, code := rig.exchange(rig.handoff(func(c *handoffClaims) { c.Session = time.Now().Add(2 * time.Second).Unix() }))
	if code != 200 {
		t.Fatalf("exchange: %d", code)
	}
	if tok.ExpiresAt.After(tok.MaxUntil) {
		t.Fatalf("expires %v after max_until %v", tok.ExpiresAt, tok.MaxUntil)
	}
	code, body := rig.post(tunnelHost, "/_access/refresh", map[string]string{"token": tok.Token}, nil)
	var again TunnelToken
	if code != 200 || json.Unmarshal(body, &again) != nil || again.Token == "" || again.ExpiresAt.After(again.MaxUntil) || !again.MaxUntil.Equal(tok.MaxUntil) {
		t.Fatalf("refresh: %d %s", code, body)
	}
	if c, _, err := rig.dial(tunnelHost, "", TunnelSubprotocol, again.Token); err != nil {
		t.Fatalf("the refreshed token: %v", err)
	} else {
		_ = c.CloseNow()
	}
	time.Sleep(2200 * time.Millisecond) // past max_until
	if code, _ := rig.post(tunnelHost, "/_access/refresh", map[string]string{"token": again.Token}, nil); code != http.StatusUnauthorized {
		t.Fatalf("refresh past max_until: %d", code)
	}
	if _, _, err := rig.dial(tunnelHost, "", TunnelSubprotocol, again.Token); err == nil {
		t.Fatal("a token past max_until opened a tunnel")
	}
}

// The tunnel is cut when the lab stops or goes, the device loses its kind, or the tenant key is removed; and a new token or tunnel is refused.
func TestTunnelIsClosedWhenAccessIsLost(t *testing.T) {
	for name, revoke := range map[string]func(*tunnelRig){
		"the lab stops": func(r *tunnelRig) {
			var l lab.Lab
			if err := r.cluster.Get(context.Background(), client.ObjectKeyFromObject(r.lab), &l); err != nil {
				r.t.Fatal(err)
			}
			l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped"}
			if err := r.cluster.Update(context.Background(), &l); err != nil {
				r.t.Fatal(err)
			}
		},
		"the lab is deleted":        func(r *tunnelRig) { _ = r.cluster.Delete(context.Background(), r.lab) },
		"the device loses its kind": func(r *tunnelRig) { _ = r.cluster.Delete(context.Background(), r.svc) },
		"the tenant key is removed": func(r *tunnelRig) { r.keyOK.Store(false) },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTunnelRig(t)
			c, tok := rig.open()
			echoOnce(t, c, "before")
			if n := rig.h.CheckLive(); n != 0 {
				t.Fatalf("closed too early: %d", n)
			}
			revoke(rig)
			if n := rig.h.CheckLive(); n != 1 {
				t.Fatalf("CheckLive closed %d, want 1", n)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, _, err := c.Read(ctx); err == nil {
				t.Fatal("the tunnel went on")
			}
			if _, _, err := rig.dial(tunnelHost, "", TunnelSubprotocol, tok.Token); err == nil {
				t.Fatal("a new tunnel opened after the access was lost")
			}
			if code, _ := rig.post(tunnelHost, "/_access/refresh", map[string]string{"token": tok.Token}, nil); code == 200 {
				t.Fatal("a refresh succeeded after the access was lost")
			}
		})
	}
}

func TestTunnelEndsAtTheAbsoluteLimit(t *testing.T) {
	rig := newTunnelRig(t)
	rig.h.WithLiveMaxLifetime(time.Second)
	c, _ := rig.open()
	echoOnce(t, c, "a")
	time.Sleep(1200 * time.Millisecond)
	if n := rig.h.CheckLive(); n != 1 {
		t.Fatalf("CheckLive closed %d after the live cap, want 1", n)
	}
}

// The bytes are counted by (group namespace, client, lab) and held to the participant's share like web traffic.
func TestTunnelIsMeteredAndPaced(t *testing.T) {
	rig := newTunnelRig(t)
	meter := NewMeter("boot", time.Now())
	rig.h.WithAccounting(meter, nil)
	c, _ := rig.open()
	payload := strings.Repeat("x", 30000)
	start := time.Now()
	echoOnce(t, c, payload)
	free := time.Since(start)
	_ = c.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	rows, _ := meter.Ledger("legacy")
	if len(rows) != 1 || rows[0].Lab != "lab" || rows[0].BytesIn < 30000 || rows[0].BytesOut < 30000 || rows[0].Attempts != 1 {
		t.Fatalf("ledger: %+v", rows)
	}
	rig.h.Throttle().SetLimits(0, 800) // 100 kB/s, burst 16 kB
	c2, _ := rig.open()
	start = time.Now()
	echoOnce(t, c2, strings.Repeat("y", 90000))
	if paced := time.Since(start); paced < 350*time.Millisecond || paced < free+250*time.Millisecond {
		t.Fatalf("180 kB through a 100 kB/s ceiling took %v (free: %v)", paced, free)
	}
}

// A graceful stop tells the client the service is restarting instead of cutting it off.
func TestTunnelDrainsOnShutdown(t *testing.T) {
	rig := newTunnelRig(t)
	c, _ := rig.open()
	echoOnce(t, c, "a")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rig.h.Shutdown(ctx) }()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status after shutdown: %v (%v)", websocket.CloseStatus(err), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// and nothing new opens
	if _, code := rig.exchange(rig.handoff(nil)); code == 200 {
		t.Error("an exchange answered while stopping")
	}
}

func TestTunnelDeviceDown(t *testing.T) {
	rig := newTunnelRig(t)
	rig.echo.Close()
	rig.h.WithTunnelDial(func(ctx context.Context, addr string) (net.Conn, error) { return nil, io.ErrClosedPipe })
	tok, _ := rig.exchange(rig.handoff(nil))
	_, resp, err := rig.dial(tunnelHost, "", TunnelSubprotocol, tok.Token)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a device that does not answer: %v %v", resp, err)
	}
}

// Other paths and methods of the host are nothing.
func TestTunnelHostServesOnlyItsThreePaths(t *testing.T) {
	rig := newTunnelRig(t)
	for _, tc := range []struct{ method, path string }{{"GET", "/"}, {"GET", "/_auth"}, {"POST", "/_tunnel"}, {"GET", "/_access/exchange"}, {"POST", "/x"}} {
		req, _ := http.NewRequest(tc.method, rig.srv.URL+tc.path, nil)
		req.Host = tunnelHost
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 404 && resp.StatusCode != 405 {
			t.Errorf("%s %s: %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}
