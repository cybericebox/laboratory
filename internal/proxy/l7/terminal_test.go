package l7

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-logr/logr"
	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
)

var termKey = bytes.Repeat([]byte("t"), 32)

const (
	termNS      = "legacy"
	termBaseDom = "challenges.example.com"
)

// termCodes are the codes of the test devices, one per (lab, device): the console host of a device is <device>-<code>t (names.AccessHostLabel).
var termCodes = map[string]string{"lab1/kali": "k10", "lab1/plain": "p10", "lab1/unplaced": "u10", "stopped/kali": "k20", "lab3/kali": "k30", "lab2/kali": "k40"}

// termHostFor is the console host of a device of the test cluster.
func termHostFor(labName, device string) string {
	label, _ := names.AccessHostLabel(device, termCodes[labName+"/"+device], names.AccessKindTerminal)
	return label + "." + termBaseDom
}

// termHost is the console host of lab1/kali, the device most tests open.
var termHost = termHostFor("lab1", "kali")

// fakeService is a device access service that answers consoles with upper-cased echo.
type fakeService struct {
	srv   *httptest.Server
	mu    sync.Mutex
	opens []wire.OpenRequest
	// ended counts the consoles the service saw end; refuse, when set, is the code every open request is answered with.
	ended  int
	refuse string
}

func newFakeService(t *testing.T) *fakeService {
	f := &fakeService{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeService) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(wire.MaxFrame)
	ctx := r.Context()
	typ, msg, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return
	}
	var req wire.OpenRequest
	_ = json.Unmarshal(msg, &req)
	f.mu.Lock()
	f.opens = append(f.opens, req)
	f.mu.Unlock()
	if !wire.VerifySignature([][]byte{termKey}, req) {
		b, _ := json.Marshal(wire.OpenReply{Error: wire.CodeUnauthorized})
		_ = c.Write(ctx, websocket.MessageText, b)
		return
	}
	reply := func(r wire.OpenReply) {
		b, _ := json.Marshal(r)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	f.mu.Lock()
	refuse := f.refuse
	f.mu.Unlock()
	if refuse != "" {
		reply(wire.OpenReply{Error: refuse})
		return
	}
	switch req.Kind {
	case wire.KindConsole:
		reply(wire.OpenReply{OK: true})
		for {
			_, m, err := c.Read(ctx)
			if err != nil {
				break
			}
			ft, p, _ := wire.Parse(m)
			if ft == wire.TypeClose {
				break
			}
			if ft == wire.TypeData {
				_ = c.Write(ctx, websocket.MessageBinary, wire.Data(bytes.ToUpper(p)))
			}
		}
		f.mu.Lock()
		f.ended++
		f.mu.Unlock()
	}
}

type termFixture struct {
	t       *testing.T
	h       *Handler
	srv     *httptest.Server
	svc     *fakeService
	cookie  string
	cluster client.Client
}

func termDevice(labName, name string, enabled bool, node string) *lab.Device {
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: devices.Name(labName, name), Namespace: termNS}}
	d.Spec.Name, d.Spec.LabRef, d.Spec.Code = name, labName, termCodes[labName+"/"+name]
	if enabled {
		d.Spec.Access = []lab.DeviceAccess{{Kind: lab.AccessTerminal, Command: []string{"/bin/bash"}, User: "kali"}}
	}
	d.Status.NodeName = node
	return d
}

func newTermFixture(t *testing.T, mutate func(cfg *TerminalConfig)) *termFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: termNS}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: termNS}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{
		{Action: lab.LabGroupAccessAllow, ClientNames: []string{"p-u1"}, LabNames: []string{"lab1", "lab2", "stopped"}}}}}
	stopped := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "stopped", Namespace: termNS}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped"}}}
	objs := []client.Object{group, policy, stopped,
		&lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-u1", Namespace: termNS}},
		&lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-u2", Namespace: termNS}},
		&lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab1", Namespace: termNS}},
		&lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab2", Namespace: termNS}},
		&lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab3", Namespace: termNS}},
		termDevice("lab1", "kali", true, "n1"),
		termDevice("lab1", "plain", false, "n1"),
		termDevice("lab1", "unplaced", true, ""),
		termDevice("stopped", "kali", true, "n1"),
		termDevice("lab3", "kali", true, "n1"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	access := &AccessReader{Reader: c}
	svc := newFakeService(t)
	h := NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge", nil).WithAccessReader(access)
	cfg := DefaultTerminalConfig()
	cfg.OriginScheme = "http"
	cfg.Log = logr.Discard()
	cfg.Devices = access.Device
	cfg.Signer = func(r *wire.OpenRequest) error { return wire.Sign(termKey, r, time.Now()) }
	cfg.Services = func(node string) (string, bool) {
		if node == "n1" {
			return svc.srv.URL, true
		}
		return "", false
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h.WithTerminal(cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &termFixture{t: t, h: h, srv: srv, svc: svc, cluster: c, cookie: cookieFor(t, "p-u1")}
}

func cookieFor(t *testing.T, clientName string) string {
	return signCookie(t, jwtClaims{GroupID: "g1", Client: clientName, Tenant: "acme",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
}

func (f *termFixture) get(path string, hdr map[string]string) *httptest.ResponseRecorder {
	return f.getHost(termHost, path, hdr)
}

func (f *termFixture) getHost(host, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://"+host+path, nil)
	req.Host = host
	if f.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "challenge", Value: f.cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *termFixture) dialSocket(device string, origin string) (*websocket.Conn, *http.Response, error) {
	return f.dialSocketHost(termHostFor("lab1", device), origin)
}

func (f *termFixture) dialSocketHost(host string, origin string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Cookie": {"challenge=" + f.cookie}}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/ws", &websocket.DialOptions{Host: host, HTTPHeader: hdr})
}

func TestTerminalPageAndHeaders(t *testing.T) {
	f := newTermFixture(t, nil)
	rec := f.get("/", nil)
	if rec.Code != 200 {
		t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'", "connect-src 'self' wss://" + termHost} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe") || strings.Contains(csp, "http") || strings.Contains(csp, "*") {
		t.Errorf("CSP is too wide: %s", csp)
	}
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if h.Get(k) != want {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `lang="uk"`) || !strings.Contains(body, "Термінал") {
		t.Errorf("not Ukrainian by default: %s", body[:200])
	}
	for _, bad := range []string{"http://", "https://", "cdn", "<script>"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page refers to %q", bad)
		}
	}
	en := f.get("/", map[string]string{"Accept-Language": "en-US,en;q=0.9"})
	if !strings.Contains(en.Body.String(), `lang="en"`) {
		t.Errorf("not English: %s", en.Body.String())
	}
	for _, name := range []string{"xterm.js", "xterm.css", "addon-fit.js", "addon-web-links.js", "terminal.js", "terminal.css"} {
		a := f.get("/assets/"+name, nil)
		if a.Code != 200 || a.Body.Len() == 0 || a.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("asset %s: %d", name, a.Code)
		}
	}
	if a := f.get("/assets/LICENSE-xterm.txt", nil); a.Code != 404 {
		t.Errorf("an unlisted file of the embedded tree was served: %d", a.Code)
	}
	if a := f.get("/assets/../x", nil); a.Code == 200 {
		t.Errorf("path escape served")
	}
}

// There is no file transfer through the console: no controls on the page, no code for it in the page script, no route.
func TestTerminalHasNoFileTransfer(t *testing.T) {
	f := newTermFixture(t, nil)
	body := f.get("/", nil).Body.String()
	js, err := terminalFS.ReadFile("terminalassets/terminal.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"upload", "download", "/files", "XMLHttpRequest", "fetch("} {
		if strings.Contains(strings.ToLower(body), bad) || strings.Contains(strings.ToLower(string(js)), strings.ToLower(bad)) {
			t.Errorf("the console page or its script mentions %q", bad)
		}
	}
	for _, path := range []string{"/files", "/files?path=/etc/passwd"} {
		for _, method := range []string{"GET", "PUT"} {
			req := httptest.NewRequest(method, "http://"+termHost+path, nil)
			req.Host = termHost
			req.AddCookie(&http.Cookie{Name: "challenge", Value: f.cookie})
			req.Header.Set("Origin", "http://"+termHost)
			rec := httptest.NewRecorder()
			f.h.ServeHTTP(rec, req)
			if rec.Code != 404 {
				t.Errorf("%s %s: %d", method, path, rec.Code)
			}
		}
	}
}

func TestTerminalNoClipboardOrTitleCode(t *testing.T) {
	js, err := terminalFS.ReadFile("terminalassets/terminal.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"onTitleChange", "ClipboardAddon", "registerOscHandler", "innerHTML", "eval("} {
		if strings.Contains(string(js), bad) {
			t.Errorf("terminal.js uses %s", bad)
		}
	}
	if !strings.Contains(string(js), "linkHandler: { activate: openLink }") || strings.Count(string(js), "window.open(") != 1 {
		t.Error("links declared by a program (OSC 8) must go through the same http(s)-only handler")
	}
	if !strings.Contains(string(js), "noopener,noreferrer") || !strings.Contains(string(js), `/^https?:\/\//i`) {
		t.Error("links are not restricted to http(s) with noopener")
	}
}

func TestTerminalAuthorizationMatrix(t *testing.T) {
	f := newTermFixture(t, nil)
	deny := func(name, host string, cookie string) {
		t.Helper()
		f.cookie = cookie
		if rec := f.getHost(host, "/", nil); rec.Code == 200 {
			t.Errorf("%s: served the page", name)
		} else if rec.Code != 404 && rec.Code != 401 && rec.Code != 503 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	good := cookieFor(t, "p-u1")
	kali := termHostFor("lab1", "kali")
	deny("no session", kali, "")
	deny("other client of the group (policy has no rule for it)", kali, cookieFor(t, "p-u2"))
	deny("unknown client", kali, cookieFor(t, "ghost"))
	deny("lab the policy does not allow", termHostFor("lab3", "kali"), good)
	deny("lab stopped", termHostFor("stopped", "kali"), good)
	deny("device without a console", termHostFor("lab1", "plain"), good)
	deny("device that does not exist", "nope-ab1t."+termBaseDom, good)
	deny("right device, wrong code", "kali-zz1t."+termBaseDom, good)
	deny("the device name alone, no code", "kali."+termBaseDom, good)
	deny("the right code but the web kind", "kali-k10w."+termBaseDom, good)
	deny("device not placed on a node yet", termHostFor("lab1", "unplaced"), good)
	// a session of another group (the group does not exist in the cluster)
	other := signCookie(t, jwtClaims{GroupID: "g-other", Client: "p-u1", Tenant: "acme", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	deny("another group", kali, other)
	// a node without the service
	f.cookie = good
	if err := f.cluster.Create(context.Background(), termDevice("lab2", "kali", true, "n2")); err != nil {
		t.Fatal(err)
	}
	deny("node without a service pod", termHostFor("lab2", "kali"), good)
	f.cookie = good
	if rec := f.get("/", nil); rec.Code != 200 {
		t.Fatalf("control: %d", rec.Code)
	}
	// The device is named by the host alone: a query or a path cannot name another one.
	if rec := f.get("/?lab=lab3&device=kali", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "lab1 / kali") {
		t.Errorf("a query chose the device: %d", rec.Code)
	}
}

// The console host of a device of lab A, with the session of a client who may not use lab A (it is allowed lab B only, in the same
// group, and a client of another group), is refused as a host that does not exist: the same card for every refusal.
func TestConsoleHostOfAnotherLabOrGroupIsRefused(t *testing.T) {
	f := newTermFixture(t, func(*TerminalConfig) {})
	ctx := context.Background()
	// the policy of the group lets p-u1 reach lab1 and lab2 only; lab3 has a console device and is not in the rules
	for _, c := range []struct{ name, host string }{{"lab3 (not in the policy)", termHostFor("lab3", "kali")}} {
		f.cookie = cookieFor(t, "p-u1")
		rec := f.getHost(c.host, "/", nil)
		if rec.Code != 404 {
			t.Errorf("%s: %d", c.name, rec.Code)
		}
		if conn, resp, err := f.dialSocketHost(c.host, "http://"+c.host); err == nil {
			_ = conn.CloseNow()
			t.Errorf("%s: the socket opened (%v)", c.name, resp)
		}
	}
	// a second group with its own namespace: the console host of group g1 is no device in g2
	g2 := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g2", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "other-ns"}}
	if err := f.cluster.Create(ctx, g2); err != nil {
		t.Fatal(err)
	}
	f.cookie = signCookie(t, jwtClaims{GroupID: "g2", Client: "p-u1", Tenant: "acme", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	if rec := f.get("/", nil); rec.Code != 404 {
		t.Errorf("the console host of group g1 with a session of g2: %d", rec.Code)
	}
	if conn, _, err := f.dialSocketHost(termHost, "http://"+termHost); err == nil {
		_ = conn.CloseNow()
		t.Error("the socket of group g1 opened for a session of g2")
	}
}

func TestConsoleHostRouting(t *testing.T) {
	f := newTermFixture(t, nil)
	// vnc, rdp, a reserved kind character and a label without a kind answer like a host that does not exist
	for _, host := range []string{"kali-k10v." + termBaseDom, "kali-k10r." + termBaseDom, "kali-k10x." + termBaseDom, "kali-k100." + termBaseDom, "kali-k10." + termBaseDom} {
		if rec := f.getHost(host, "/", nil); rec.Code != 404 {
			t.Errorf("%s: %d", host, rec.Code)
		}
	}
	// a web host is never a console: the label without the mark is looked up as a Service, and there is none
	if rec := f.getHost("kali-k10w."+termBaseDom, "/", nil); rec.Code != 404 {
		t.Errorf("the web host of a console device served: %d", rec.Code)
	}
	// the old reserved host is gone
	if rec := f.getHost("terminal."+termBaseDom, "/", nil); rec.Code != 404 {
		t.Errorf("the old reserved host served: %d", rec.Code)
	}
	// the page and the socket are on the host of the device
	f.openConsole("kali")
	other := termHostFor("lab1", "plain")
	// Origin must be the host's own origin: the console host of another device of the same lab is another origin
	if conn, _, err := f.dialSocketHost(termHost, "http://"+other); err == nil {
		_ = conn.CloseNow()
		t.Error("the socket opened for the origin of another device's console host")
	}
	// there is no file transfer on a console host at all
	if rec := f.get("/files?path=/tmp/a", nil); rec.Code != 404 {
		t.Errorf("/files answered %d", rec.Code)
	}
}

func TestTerminalHostIsDisabledByDefault(t *testing.T) {
	// A handler without WithTerminal serves no console on a console host.
	f := newTermFixture(t, nil)
	f.h.terminal = nil
	if rec := f.get("/", nil); rec.Code != 404 {
		t.Fatalf("console served while off: %d", rec.Code)
	}
}

func readBinary(t *testing.T, c *websocket.Conn) (byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, msg, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		t.Fatalf("read: %v %v", typ, err)
	}
	ft, p, err := wire.Parse(msg)
	if err != nil {
		t.Fatal(err)
	}
	return ft, append([]byte(nil), p...)
}

func readText(t *testing.T, c *websocket.Conn) browserMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, msg, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("read text: %v %v", typ, err)
	}
	var m browserMsg
	_ = json.Unmarshal(msg, &m)
	return m
}

// openConsole opens a console and returns the connection.
func (f *termFixture) openConsole(device string) *websocket.Conn {
	f.t.Helper()
	c, _, err := f.dialSocket(device, "http://"+termHostFor("lab1", device))
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	f.t.Cleanup(func() { _ = c.CloseNow() })
	c.SetReadLimit(wire.MaxFrame)
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"cols":100,"rows":30}`)); err != nil {
		f.t.Fatal(err)
	}
	m := readText(f.t, c)
	if m.Type != "ready" {
		f.t.Fatalf("not ready: %+v", m)
	}
	return c
}

func TestTerminalConsoleRelay(t *testing.T) {
	f := newTermFixture(t, nil)
	c := f.openConsole("kali")
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageBinary, wire.Data([]byte("id\n"))); err != nil {
		t.Fatal(err)
	}
	ft, p := readBinary(t, c)
	if ft != wire.TypeData || string(p) != "ID\n" {
		t.Fatalf("got %d %q", ft, p)
	}
	_ = c.Write(ctx, websocket.MessageBinary, wire.Resize(120, 40))
	_ = c.Write(ctx, websocket.MessageBinary, wire.Ack(3))
	_ = c.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonClient))
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.svc.mu.Lock()
		ended := f.svc.ended
		f.svc.mu.Unlock()
		if ended == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the service stream was not closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// the open request names a device and carries the terminal settings of the lab, and no address
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	o := f.svc.opens[0]
	if o.Kind != wire.KindConsole || o.Namespace != termNS || o.Lab != "lab1" || o.Device != "kali" || o.User != "kali" || len(o.Command) != 1 || o.Cols != 100 || o.Rows != 30 {
		t.Fatalf("open: %+v", o)
	}
	if o.Client != "p-u1" || o.Group != "g1" {
		t.Fatalf("who: %+v", o)
	}
}

func TestTerminalOriginMustBeThePagesOwn(t *testing.T) {
	f := newTermFixture(t, nil)
	for name, origin := range map[string]string{
		"none":                  "",
		"a device of the lab":   "http://kali-ab12cd.challenges.example.com",
		"another site":          "http://evil.example.net",
		"the right host, https": "https://" + termHost,
		"a host with a suffix":  "http://" + termHost + ".evil.net",
		"null":                  "null",
	} {
		c, resp, err := f.dialSocket("kali", origin)
		if err == nil {
			_ = c.CloseNow()
			t.Errorf("%s: the handshake succeeded", name)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %v %v", name, resp, err)
		}
	}
	if _, resp, err := f.dialSocket("kali", "http://"+termHost); err != nil {
		t.Fatalf("own origin refused: %v %v", err, resp)
	}
}

func TestTerminalSocketAuthorization(t *testing.T) {
	f := newTermFixture(t, nil)
	for _, device := range []string{"plain", "nope", "unplaced"} {
		c, resp, err := f.dialSocket(device, "http://"+termHostFor("lab1", device))
		if err == nil {
			_ = c.CloseNow()
			t.Errorf("%s: socket opened", device)
		} else if resp == nil || resp.StatusCode == 101 {
			t.Errorf("%s: %v", device, err)
		}
	}
}

func TestTerminalConsolesPerClient(t *testing.T) {
	f := newTermFixture(t, func(c *TerminalConfig) { c.PerClient = 2 })
	f.openConsole("kali")
	f.openConsole("kali")
	c, resp, err := f.dialSocket("kali", "http://"+termHost)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("a third console opened")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("%v %v", resp, err)
	}
}

func TestTerminalLiveCheckClosesAConsoleWhenAccessIsLost(t *testing.T) {
	f := newTermFixture(t, nil)
	c := f.openConsole("kali")
	if n := f.h.CheckLive(); n != 0 {
		t.Fatalf("closed %d while allowed", n)
	}
	// the policy no longer allows the client
	ctx := context.Background()
	var pol lab.LabGroupAccessPolicy
	if err := f.cluster.Get(ctx, client.ObjectKey{Namespace: termNS, Name: names.LabGroupAccessPolicyName}, &pol); err != nil {
		t.Fatal(err)
	}
	pol.Spec.Rules = nil
	if err := f.cluster.Update(ctx, &pol); err != nil {
		t.Fatal(err)
	}
	if n := f.h.CheckLive(); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		typ, msg, err := c.Read(rctx)
		if err != nil {
			break // closed
		}
		if typ == websocket.MessageBinary {
			if ft, p, _ := wire.Parse(msg); ft == wire.TypeClose && string(p) != wire.ReasonAccess {
				t.Fatalf("closed with %q", p)
			}
		}
	}
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for f.svc.ended == 0 && time.Now().Before(deadline) {
		f.svc.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		f.svc.mu.Lock()
	}
	if f.svc.ended != 1 {
		t.Fatal("the service stream was not ended")
	}
}

func TestTerminalFlowControlHoldsTheShellBack(t *testing.T) {
	f := newTermFixture(t, nil)
	c := f.openConsole("kali")
	ctx := context.Background()
	chunk := bytes.Repeat([]byte("a"), wire.MaxData)
	// the fake service answers each data frame with the same size; send more than the window without reading or acknowledging
	for range 40 {
		if err := c.Write(ctx, websocket.MessageBinary, wire.Data(chunk)); err != nil {
			break
		}
	}
	// read what was let through: at most the window (and what is in flight in the pipes)
	got := 0
	for {
		rctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		_, msg, err := c.Read(rctx)
		cancel()
		if err != nil {
			break
		}
		if ft, p, _ := wire.Parse(msg); ft == wire.TypeData {
			got += len(p)
		}
	}
	if got > outputWindow+wire.MaxData {
		t.Fatalf("%d bytes of output went to a browser that acknowledged none (window %d)", got, outputWindow)
	}
	if got < outputWindow-wire.MaxData {
		t.Fatalf("only %d bytes, the window was not used", got)
	}
}

func TestTerminalServiceRefusalReachesTheBrowserAsACode(t *testing.T) {
	for _, code := range []string{"disabled", "not-found", "limit", "something-else"} {
		f := newTermFixture(t, nil)
		f.svc.mu.Lock()
		f.svc.refuse = code
		f.svc.mu.Unlock()
		c, _, err := f.dialSocket("kali", "http://"+termHost)
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadLimit(wire.MaxFrame)
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"cols":80,"rows":24}`))
		m := readText(t, c)
		want := code
		if code == "something-else" {
			want = "unavailable" // an unknown code is not passed on
		}
		if m.Type != "error" || m.Code != want {
			t.Errorf("%s: %+v", code, m)
		}
		_ = c.CloseNow()
	}
}

func TestTerminalBadFirstMessage(t *testing.T) {
	f := newTermFixture(t, nil)
	for _, first := range []string{`{"cols":0,"rows":24}`, `{"cols":9999,"rows":24}`, `not json`, `{}`} {
		c, _, err := f.dialSocket("kali", "http://"+termHost)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Write(context.Background(), websocket.MessageText, []byte(first))
		c.SetReadLimit(wire.MaxFrame)
		if m := readText(t, c); m.Type != "error" || m.Code != "bad-request" {
			t.Errorf("%s: %+v", first, m)
		}
		_ = c.CloseNow()
	}
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.opens) != 0 {
		t.Fatal("the service was asked to open a console for a bad request")
	}
}

func TestTerminalIdleClosesTheConsole(t *testing.T) {
	f := newTermFixture(t, func(c *TerminalConfig) { c.Idle = 150 * time.Millisecond })
	c := f.openConsole("kali")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no close frame: %v", err)
		}
		if typ == websocket.MessageBinary {
			if ft, p, _ := wire.Parse(msg); ft == wire.TypeClose {
				if string(p) != wire.ReasonIdle {
					t.Fatalf("closed with %q", p)
				}
				return
			}
		}
	}
}

// Every open request the proxy sends carries a signature the service accepts; without a key the console does not open.
func TestTerminalSignsEveryOpenRequest(t *testing.T) {
	f := newTermFixture(t, nil)
	f.openConsole("kali")
	f.openConsole("kali")
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.opens) != 2 {
		t.Fatalf("%d opens", len(f.svc.opens))
	}
	for _, o := range f.svc.opens {
		if !wire.VerifySignature([][]byte{termKey}, o) || o.Nonce == "" || o.Time == 0 {
			t.Errorf("an open request is not signed: %+v", o)
		}
	}
	if f.svc.opens[0].Nonce == f.svc.opens[1].Nonce {
		t.Error("a nonce was reused")
	}
}

func TestTerminalWithoutAKeyDoesNotOpen(t *testing.T) {
	f := newTermFixture(t, func(c *TerminalConfig) { c.Signer = nil })
	c, _, err := f.dialSocket("kali", "http://"+termHost)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(wire.MaxFrame)
	_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"cols":80,"rows":24}`))
	if m := readText(t, c); m.Type != "error" {
		t.Fatalf("%+v", m)
	}
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.opens) != 0 {
		t.Fatal("an unsigned request was sent")
	}
}

func TestEndpointDirectory(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = discoveryv1.AddToScheme(scheme)
	ready, notReady, terminating := true, false, true
	node := func(s string) *string { return &s }
	slice := func(name, svc string, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "da", Labels: map[string]string{ServiceNameLabel: svc}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: eps}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		slice("a", names.DeviceAccessApp,
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, NodeName: node("n1"), Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}, NodeName: node("n2"), Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}, NodeName: node("n3"), Conditions: discoveryv1.EndpointConditions{Ready: &ready, Terminating: &terminating}},
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.4"}, NodeName: node("n4")},
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}),
		slice("other", "something-else", discoveryv1.Endpoint{Addresses: []string{"10.9.9.9"}, NodeName: node("n9"), Conditions: discoveryv1.EndpointConditions{Ready: &ready}}),
	).Build()
	dir := EndpointDirectory(c, "da", names.DeviceAccessApp, 8080)
	for node, want := range map[string]string{"n1": "http://10.0.0.1:8080", "n4": "http://10.0.0.4:8080", "n2": "", "n3": "", "n9": "", "": "", "nx": ""} {
		got, ok := dir(node)
		if got != want || ok != (want != "") {
			t.Errorf("node %q: %q %v, want %q", node, got, ok, want)
		}
	}
}
