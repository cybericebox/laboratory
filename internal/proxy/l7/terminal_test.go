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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
)

const (
	termHost = "terminal.challenges.example.com"
	termNS   = "legacy"
)

// fakeService is a device access service that answers consoles with upper-cased echo and keeps uploaded files.
type fakeService struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	opens []wire.OpenRequest
	// ended counts the consoles the service saw end; refuse, when set, is the code every open request is answered with.
	ended  int
	refuse string
}

func newFakeService(t *testing.T) *fakeService {
	f := &fakeService{files: map[string][]byte{}}
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
	case wire.KindPut:
		reply(wire.OpenReply{OK: true})
		var buf bytes.Buffer
		for {
			_, m, err := c.Read(ctx)
			if err != nil {
				return
			}
			ft, p, _ := wire.Parse(m)
			if ft == wire.TypeData {
				buf.Write(p)
				continue
			}
			break
		}
		f.mu.Lock()
		f.files[req.Path] = buf.Bytes()
		f.mu.Unlock()
		reply(wire.OpenReply{OK: true})
	case wire.KindGet:
		f.mu.Lock()
		data, ok := f.files[req.Path]
		f.mu.Unlock()
		if !ok {
			reply(wire.OpenReply{Error: "not-found"})
			return
		}
		reply(wire.OpenReply{OK: true, Size: int64(len(data))})
		for b := data; len(b) > 0; {
			n := min(len(b), wire.MaxData)
			_ = c.Write(ctx, websocket.MessageBinary, wire.Data(b[:n]))
			b = b[n:]
		}
		_ = c.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonExit))
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
	d.Spec.Name, d.Spec.LabRef = name, labName
	if enabled {
		d.Spec.Terminal = &lab.DeviceTerminal{Enabled: true, Command: []string{"/bin/bash"}, User: "kali"}
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
	req := httptest.NewRequest("GET", "http://"+termHost+path, nil)
	req.Host = termHost
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Cookie": {"challenge=" + f.cookie}}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/ws?lab=lab1&device="+device, &websocket.DialOptions{Host: termHost, HTTPHeader: hdr})
}

func TestTerminalPageAndHeaders(t *testing.T) {
	f := newTermFixture(t, nil)
	rec := f.get("/?lab=lab1&device=kali", nil)
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
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "Cache-Control": "no-store"} {
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
	en := f.get("/?lab=lab1&device=kali", map[string]string{"Accept-Language": "en-US,en;q=0.9"})
	if !strings.Contains(en.Body.String(), `lang="en"`) || !strings.Contains(en.Body.String(), "Upload to the device") {
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
	if !strings.Contains(string(js), "noopener,noreferrer") || !strings.Contains(string(js), `/^https?:\/\//i`) {
		t.Error("links are not restricted to http(s) with noopener")
	}
}

func TestTerminalAuthorizationMatrix(t *testing.T) {
	f := newTermFixture(t, nil)
	deny := func(name, path string, cookie string) {
		t.Helper()
		f.cookie = cookie
		if rec := f.get(path, nil); rec.Code == 200 {
			t.Errorf("%s: served the page", name)
		} else if rec.Code != 404 && rec.Code != 401 && rec.Code != 503 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	good := cookieFor(t, "p-u1")
	deny("no session", "/?lab=lab1&device=kali", "")
	deny("other client of the group (policy has no rule for it)", "/?lab=lab1&device=kali", cookieFor(t, "p-u2"))
	deny("unknown client", "/?lab=lab1&device=kali", cookieFor(t, "ghost"))
	deny("lab the policy does not allow", "/?lab=lab3&device=kali", good)
	deny("lab stopped", "/?lab=stopped&device=kali", good)
	deny("lab that does not exist", "/?lab=nope&device=kali", good)
	deny("device without a console", "/?lab=lab1&device=plain", good)
	deny("device that does not exist", "/?lab=lab1&device=nope", good)
	deny("no lab", "/?device=kali", good)
	deny("path-like names", "/?lab=../x&device=kali", good)
	deny("device not placed on a node yet", "/?lab=lab1&device=unplaced", good)
	// a session of another group (the group does not exist in the cluster)
	other := signCookie(t, jwtClaims{GroupID: "g-other", Client: "p-u1", Tenant: "acme", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	deny("another group", "/?lab=lab1&device=kali", other)
	// a node without the service
	f.cookie = good
	if err := f.cluster.Create(context.Background(), termDevice("lab2", "kali", true, "n2")); err != nil {
		t.Fatal(err)
	}
	deny("node without a service pod", "/?lab=lab2&device=kali", good)
	f.cookie = good
	if rec := f.get("/?lab=lab1&device=kali", nil); rec.Code != 200 {
		t.Fatalf("control: %d", rec.Code)
	}
}

func TestTerminalHostIsReservedAndDisabledByDefault(t *testing.T) {
	// A handler without WithTerminal treats the host as any other task host: no console.
	f := newTermFixture(t, nil)
	f.h.terminal = nil
	if rec := f.get("/?lab=lab1&device=kali", nil); rec.Code != 404 {
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

// openConsole opens a console and returns the connection and the session token.
func (f *termFixture) openConsole(device string) (*websocket.Conn, string) {
	f.t.Helper()
	c, _, err := f.dialSocket(device, "http://"+termHost)
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
	if m.Type != "ready" || len(m.Session) < 16 {
		f.t.Fatalf("not ready: %+v", m)
	}
	return c, m.Session
}

func TestTerminalConsoleRelay(t *testing.T) {
	f := newTermFixture(t, nil)
	c, _ := f.openConsole("kali")
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
		c, resp, err := f.dialSocket(device, "http://"+termHost)
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
	c, _ := f.openConsole("kali")
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
	c, _ := f.openConsole("kali")
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

// ---- files ----

func (f *termFixture) put(token, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "http://"+termHost+"/files?session="+token+"&path="+path, bytes.NewReader(body))
	req.Host = termHost
	req.AddCookie(&http.Cookie{Name: "challenge", Value: f.cookie})
	req.Header.Set("Origin", "http://"+termHost)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestTerminalUploadAndDownload(t *testing.T) {
	f := newTermFixture(t, nil)
	_, token := f.openConsole("kali")
	content := bytes.Repeat([]byte("0123456789"), 4000)
	if rec := f.put(token, "/tmp/a.bin", content, nil); rec.Code != 204 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	f.svc.mu.Lock()
	got := f.svc.files["/tmp/a.bin"]
	f.svc.mu.Unlock()
	if !bytes.Equal(got, content) {
		t.Fatalf("the service got %d bytes", len(got))
	}
	rec := f.get("/files?session="+token+"&path=/tmp/a.bin", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("download: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment;") || h.Get("Content-Length") != "40000" {
		t.Fatalf("headers: %v", h)
	}
	// the transfers spent the console's budget: 2 x 40000 of 64 MiB
	sess := f.h.terminal.session(token)
	if used := f.h.terminal.cfg.MaxSession - sess.budget.Load(); used != 80000 {
		t.Fatalf("budget used %d", used)
	}
}

func TestTerminalFileRefusals(t *testing.T) {
	f := newTermFixture(t, func(c *TerminalConfig) { c.MaxFile = 1000; c.MaxSession = 1500 })
	_, token := f.openConsole("kali")

	if rec := f.put("0000", "/tmp/a", []byte("x"), nil); rec.Code != 404 {
		t.Errorf("unknown token: %d", rec.Code)
	}
	if rec := f.put(token, "/tmp/a", []byte("x"), map[string]string{"Origin": "http://evil.example.net"}); rec.Code != 403 {
		t.Errorf("foreign origin: %d", rec.Code)
	}
	if rec := f.put(token, "/tmp/a", []byte("x"), map[string]string{"Origin": ""}); rec.Code != 403 {
		t.Errorf("no origin: %d", rec.Code)
	}
	if rec := f.put(token, "/tmp/a", bytes.Repeat([]byte("x"), 1001), nil); rec.Code != 413 {
		t.Errorf("over the file limit: %d", rec.Code)
	}
	for _, p := range []string{"relative", "/", "/tmp/../etc/passwd", "/tmp//x", "/tmp/a\nb", ""} {
		if rec := f.put(token, strings.ReplaceAll(p, "\n", "%0A"), []byte("x"), nil); rec.Code != 422 {
			t.Errorf("path %q: %d", p, rec.Code)
		}
	}
	// 1000 fits, a second 1000 does not (1500 per console); a refused one gives its bytes back
	if rec := f.put(token, "/tmp/one", bytes.Repeat([]byte("x"), 1000), nil); rec.Code != 204 {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := f.put(token, "/tmp/two", bytes.Repeat([]byte("x"), 1000), nil); rec.Code != 413 {
		t.Errorf("over the session limit: %d", rec.Code)
	}
	if rec := f.put(token, "/tmp/three", bytes.Repeat([]byte("x"), 500), nil); rec.Code != 204 {
		t.Errorf("what is left of the budget: %d", rec.Code)
	}
	// downloads
	if rec := f.get("/files?session="+token+"&path=/missing", nil); rec.Code != 404 {
		t.Errorf("missing file: %d", rec.Code)
	}
	if rec := f.get("/files?session="+token+"&path=/tmp/one", map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Errorf("cross-site download: %d", rec.Code)
	}
	// the token is of one client: another session of the group cannot use it
	f.cookie = cookieFor(t, "p-u2")
	if rec := f.get("/files?session="+token+"&path=/tmp/one", nil); rec.Code == 200 {
		t.Errorf("the token worked for another client")
	}
}

func TestTerminalFileNeedsAnOpenConsole(t *testing.T) {
	f := newTermFixture(t, nil)
	c, token := f.openConsole("kali")
	_ = c.Write(context.Background(), websocket.MessageBinary, wire.Close(wire.ReasonClient))
	deadline := time.Now().Add(2 * time.Second)
	for f.h.terminal.session(token) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rec := f.put(token, "/tmp/a", []byte("x"), nil); rec.Code != 404 {
		t.Fatalf("a closed console's token worked: %d", rec.Code)
	}
}

func TestDownloadName(t *testing.T) {
	for in, want := range map[string]string{
		"/tmp/report.txt":                  "report.txt",
		"/tmp/a b;c\"d":                    "a_b_c_d",
		"/tmp/.hidden":                     "hidden",
		"/tmp/..":                          "download",
		"/tmp/x\r\nSet-Cookie: a=":         "x__Set-Cookie__a_",
		"/tmp/файл.txt":                    "____.txt",
		"/":                                "download",
		"/tmp/" + strings.Repeat("a", 300): strings.Repeat("a", 100),
	} {
		if got := downloadName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSessionBudgetIsNeverNegative(t *testing.T) {
	var s consoleSession
	s.budget.Store(100)
	if !s.reserve(60) || s.reserve(60) || !s.reserve(40) || s.reserve(1) {
		t.Fatal("reserve")
	}
	s.refund(10)
	if !s.reserve(10) {
		t.Fatal("refund")
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
	c, _ := f.openConsole("kali")
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
