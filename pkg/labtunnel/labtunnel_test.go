package labtunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/proxy/l7"
)

const (
	host   = "bot-k7xg.challenges.example.com"
	secret = "test-session-secret-0123456789abcdef"
)

// pki is a CA and the two certificates of an mTLS device: the proxy never sees any of it.
type pki struct {
	pool       *x509.CertPool
	server     tls.Certificate
	client     tls.Certificate
	clientName string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "device-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, cn string, usage x509.ExtKeyUsage) tls.Certificate {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn}, NotBefore: time.Now().Add(-time.Hour),
			NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{pool: pool, server: issue(2, "device.internal", x509.ExtKeyUsageServerAuth), client: issue(3, "runner-client", x509.ExtKeyUsageClientAuth), clientName: "runner-client"}
}

// proxy is one replica of the lab proxy in front of the device. It can be stopped and started again on the same
// address (a restart).
type proxy struct {
	t       *testing.T
	addr    string
	secret  string
	keysOK  *atomic.Bool
	pub     ed25519.PublicKey
	device  string
	srv     *httptest.Server
	handler *l7.Handler
	ttl     time.Duration
	exch    atomic.Int32
	refresh atomic.Int32
	tunnels atomic.Int32
}

func (p *proxy) start() {
	p.t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "bot-k7xg", Namespace: "legacy",
		Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "bot", names.LabelAccessKind: "grpc"}},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Name: "grpc", Port: 9099, TargetPort: intstr.FromInt32(9099), Protocol: corev1.ProtocolTCP}}}}
	cluster := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, svc, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}).Build()
	keys := func(tenant, kid string) (ed25519.PublicKey, bool) {
		if tenant == "acme" && kid == "k1" && p.keysOK.Load() {
			return p.pub, true
		}
		return nil, false
	}
	h := l7.NewHandler(keys, []byte(p.secret), "challenges.example.com", "challenge", nil).
		WithAccessReader(&l7.AccessReader{Reader: cluster}).
		WithTunnelDial(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", p.device)
		})
	if p.ttl > 0 {
		h.WithTunnelTokenTTL(p.ttl)
	}
	p.handler = h
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_access/exchange":
			p.exch.Add(1)
		case "/_access/refresh":
			p.refresh.Add(1)
		case "/_tunnel":
			p.tunnels.Add(1)
		}
		h.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.srv = httptest.NewUnstartedServer(counted)
	p.srv.Listener.Close()
	p.srv.Listener = ln
	p.srv.Start()
	p.addr = ln.Addr().String()
}

// stop is a restart of the replica: the tunnels are closed, the listener goes.
func (p *proxy) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.handler.Shutdown(ctx)
	p.srv.CloseClientConnections()
	p.srv.Close()
}

// clock is the client's idea of the time: the proxy keeps real time, so a test moves only the client to reach "the
// token is about to end".
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

type env struct {
	t       *testing.T
	pki     *pki
	proxy   *proxy
	priv    ed25519.PrivateKey
	handoff atomic.Int32
	session time.Duration // how long the session of a handoff lasts
	peers   chan string
}

// newEnv starts the mTLS gRPC device and one proxy replica in front of it.
func newEnv(t *testing.T) *env {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e := &env{t: t, pki: newPKI(t), priv: priv, session: 24 * time.Hour, peers: make(chan string, 16)}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{e.pki.server}, ClientCAs: e.pki.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	})), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		var in wrapperspb.StringValue
		if err := stream.RecvMsg(&in); err != nil {
			return err
		}
		if p, ok := peer.FromContext(stream.Context()); ok {
			if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
				select {
				case e.peers <- ti.State.PeerCertificates[0].Subject.CommonName:
				default:
				}
			}
		}
		return stream.SendMsg(wrapperspb.String("device:" + in.Value))
	}))
	dl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(dl) }()
	t.Cleanup(srv.Stop)
	keysOK := &atomic.Bool{}
	keysOK.Store(true)
	e.proxy = &proxy{t: t, addr: "127.0.0.1:0", secret: secret, keysOK: keysOK, pub: pub, device: dl.Addr().String()}
	e.proxy.start()
	t.Cleanup(func() {
		if e.proxy.srv != nil {
			e.proxy.stop()
		}
	})
	return e
}

// getHandoff is what the caller of the library supplies: the platform's 60 s link for the device host.
func (e *env) getHandoff(ctx context.Context) (string, error) {
	e.handoff.Add(1)
	now := time.Now()
	claims := jwt.MapClaims{"iss": "acme", "sub": "run-1", "aud": []string{"laboratory-proxy"}, "group_id": "g1", "host": host, "use": "tunnel",
		"sess": now.Add(e.session).Unix(), "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = "k1"
	return tok.SignedString(e.priv)
}

func (e *env) dialer(opts ...Option) func(ctx context.Context, addr string) (net.Conn, error) {
	return Dialer(e.getHandoff, append([]Option{WithEndpoint("http://" + e.proxy.addr)}, opts...)...)
}

func (e *env) grpcConn(dial func(ctx context.Context, addr string) (net.Conn, error)) *grpc.ClientConn {
	e.t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+host+":443", grpc.WithContextDialer(dial), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{e.pki.client}, RootCAs: e.pki.pool, ServerName: "device.internal", MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func call(conn *grpc.ClientConn, in string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var out wrapperspb.StringValue
	err := conn.Invoke(ctx, "/test.Device/Echo", wrapperspb.String(in), &out, grpc.WaitForReady(true))
	return out.Value, err
}

// The application's own mTLS runs end to end: the device sees the client certificate, the proxy only carries bytes.
func TestMutualTLSEndToEndThroughTheTunnel(t *testing.T) {
	e := newEnv(t)
	conn := e.grpcConn(e.dialer())
	got, err := call(conn, "hi")
	if err != nil || got != "device:hi" {
		t.Fatalf("call: %q %v", got, err)
	}
	select {
	case cn := <-e.peers:
		if cn != e.pki.clientName {
			t.Fatalf("the device saw %q, want the client certificate %q", cn, e.pki.clientName)
		}
	case <-time.After(time.Second):
		t.Fatal("the device saw no client certificate")
	}
	// a client with the wrong certificate is refused by the DEVICE, not by the proxy
	bad := newPKI(t)
	wrong, err := grpc.NewClient("passthrough:///"+host+":443", grpc.WithContextDialer(e.dialer()), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{bad.client}, RootCAs: e.pki.pool, ServerName: "device.internal", MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out wrapperspb.StringValue
	if err := wrong.Invoke(ctx, "/test.Device/Echo", wrapperspb.String("x"), &out); status.Code(err) == codes.OK {
		t.Fatal("a client certificate of another CA was accepted")
	}
	if e.handoff.Load() != 2 { // one per Dialer: the exchange is made once per dialer and host
		t.Logf("handoffs: %d", e.handoff.Load())
	}
}

// The token is refreshed before it ends, without a new handoff, and the refreshed one opens the tunnel.
func TestTokenIsRefreshedBeforeItExpires(t *testing.T) {
	e := newEnv(t)
	e.proxy.stop()
	e.proxy.ttl = 20 * time.Second
	e.proxy.start()
	clk := &clock{}
	dial := e.dialer(withClock(clk.now))
	c, err := dial(context.Background(), host+":443")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if e.handoff.Load() != 1 || e.proxy.exch.Load() != 1 || e.proxy.refresh.Load() != 0 {
		t.Fatalf("first dial: handoffs %d exchanges %d refreshes %d", e.handoff.Load(), e.proxy.exch.Load(), e.proxy.refresh.Load())
	}
	clk.offset.Store(int64(17 * time.Second)) // the last quarter of the 20 s token (the proxy still holds it valid)
	c, err = dial(context.Background(), host+":443")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if e.handoff.Load() != 1 || e.proxy.refresh.Load() != 1 {
		t.Fatalf("second dial: handoffs %d refreshes %d, want 1 and 1", e.handoff.Load(), e.proxy.refresh.Load())
	}
	// while the token is good, nothing is asked at all
	before := e.proxy.refresh.Load()
	c, _ = dial(context.Background(), host+":443")
	_ = c.Close()
	if e.proxy.refresh.Load() != before || e.proxy.exch.Load() != 1 {
		t.Fatalf("an unneeded round trip: refreshes %d exchanges %d", e.proxy.refresh.Load(), e.proxy.exch.Load())
	}
}

// A proxy replica restarts: the tunnel drops, gRPC dials again, and the dialer reconnects with the token it holds,
// without a new handoff.
func TestReconnectsAfterAProxyRestart(t *testing.T) {
	e := newEnv(t)
	conn := e.grpcConn(e.dialer())
	if got, err := call(conn, "one"); err != nil || got != "device:one" {
		t.Fatalf("before: %q %v", got, err)
	}
	e.proxy.stop() // the tunnel is cut, the streams in flight with it
	if _, err := func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var out wrapperspb.StringValue
		return out.Value, conn.Invoke(ctx, "/test.Device/Echo", wrapperspb.String("x"), &out)
	}(); err == nil {
		t.Fatal("a call went through a stopped proxy")
	}
	e.proxy.start() // another replica, the same secret
	if got, err := call(conn, "two"); err != nil || got != "device:two" {
		t.Fatalf("after the restart: %q %v", got, err)
	}
	if e.handoff.Load() != 1 {
		t.Fatalf("the reconnect asked for %d handoffs, want the one of the first connection", e.handoff.Load())
	}
}

// A proxy that does not know the token (its secret is another) answers 401; the dialer then starts again from a
// handoff.
func TestAskForANewHandoffAfter401(t *testing.T) {
	e := newEnv(t)
	dial := e.dialer()
	c, err := dial(context.Background(), host+":443")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	e.proxy.stop()
	e.proxy.secret = "another-secret-another-secret-123456"
	e.proxy.start()
	c, err = dial(context.Background(), host+":443")
	if err != nil {
		t.Fatalf("after the secret changed: %v", err)
	}
	_ = c.Close()
	if e.handoff.Load() != 2 {
		t.Fatalf("handoffs %d, want 2", e.handoff.Load())
	}
}

// Past max_until the token cannot be refreshed: a new handoff is asked for; when the platform refuses, the dial fails
// with that error.
func TestExpiryAsksForANewHandoff(t *testing.T) {
	e := newEnv(t)
	e.session = 2 * time.Minute
	clk := &clock{}
	dial := e.dialer(withClock(clk.now))
	c, err := dial(context.Background(), host+":443")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	clk.offset.Store(int64(3 * time.Minute)) // past the session end: max_until
	e.session = time.Hour
	c, err = dial(context.Background(), host+":443")
	if err != nil {
		t.Fatalf("after max_until: %v", err)
	}
	_ = c.Close()
	if e.handoff.Load() != 2 || e.proxy.exch.Load() != 2 {
		t.Fatalf("handoffs %d exchanges %d, want 2 and 2", e.handoff.Load(), e.proxy.exch.Load())
	}

	refused := errors.New("the platform says no")
	denied := Dialer(func(context.Context) (string, error) { return "", refused }, WithEndpoint("http://"+e.proxy.addr))
	if _, err := denied(context.Background(), host+":443"); !errors.Is(err, refused) {
		t.Fatalf("a refused handoff: %v", err)
	}
	// a link the proxy does not accept (the key was removed) is an error of the exchange, with its status
	e.proxy.keysOK.Store(false)
	bad := Dialer(e.getHandoff, WithEndpoint("http://"+e.proxy.addr))
	_, err = bad(context.Background(), host+":443")
	var he *HTTPError
	if !errors.As(err, &he) || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a link of a removed key: %v", err)
	}
}

// The connection is a net.Conn: deadlines and Close behave as on TCP.
func TestConnectionSemantics(t *testing.T) {
	e := newEnv(t)
	c, err := e.dialer()(context.Background(), host+":443")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = c.Read(make([]byte, 16)) // the device waits for a TLS hello and sends nothing first
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a read deadline: %v", err)
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if c.LocalAddr() == nil || c.RemoteAddr() == nil {
		t.Fatal("no addresses")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("a write after Close succeeded")
	}
	// a cancelled context stops the dial
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.dialer()(ctx, host+":443"); err == nil {
		t.Fatal("a cancelled dial succeeded")
	}
}
