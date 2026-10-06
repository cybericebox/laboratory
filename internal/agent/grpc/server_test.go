package grpc

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// serverRig starts the real server (the split listener, the Enroll server and the main one) for the enrollment rig.
type serverRig struct {
	r    *enrollRig
	srv  *Server
	addr string
	pool *x509.CertPool
}

func startServer(t *testing.T, mod func(*config.Config)) *serverRig {
	t.Helper()
	return startServerWith(t, mod, nil)
}

// startServerWith serves impl instead of the rig's handler (nil: the handler).
func startServerWith(t *testing.T, mod func(*config.Config), impl protobuf.LabManagerServer) *serverRig {
	t.Helper()
	r := newEnrollRig(t)
	dir := t.TempDir()
	skey := newECKey(t)
	_, caKey, _, err := loadCA(r.h.caCertFile, r.h.caKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	stmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "agent"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: r.now.Add(-time.Hour), NotAfter: r.now.Add(time.Hour * 24 * 30),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	sder, _ := x509.CreateCertificate(rand.Reader, stmpl, r.ca, &skey.PublicKey, caKey)
	skder, _ := x509.MarshalECPrivateKey(skey)
	srvCrt, srvKey := filepath.Join(dir, "s.crt"), filepath.Join(dir, "s.key")
	_ = os.WriteFile(srvCrt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sder}), 0o600)
	_ = os.WriteFile(srvKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: skder}), 0o600)

	cfg := &config.Config{}
	cfg.ServerTLS = config.ServerTLSConfig{CertFile: srvCrt, KeyFile: srvKey}
	cfg.MTLS = config.MTLSConfig{ClientCAFile: r.h.caCertFile}
	if mod != nil {
		mod(cfg)
	}
	if impl == nil {
		impl = r.h
	}
	srv, err := New(cfg, impl)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	pool := x509.NewCertPool()
	pool.AddCert(r.ca)
	return &serverRig{r: r, srv: srv, addr: lis.Addr().String(), pool: pool}
}

func (s *serverRig) tlsConfig(certs ...tls.Certificate) *tls.Config {
	return &tls.Config{RootCAs: s.pool, Certificates: certs, ServerName: "localhost", NextProtos: []string{"h2"}, Time: func() time.Time { return s.r.now }}
}

func (s *serverRig) dial(t *testing.T, certs ...tls.Certificate) protobuf.LabManagerClient {
	t.Helper()
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(credentials.NewTLS(s.tlsConfig(certs...))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return protobuf.NewLabManagerClient(conn)
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// R-4: a caller without a client certificate reaches the Enroll server, which reads small messages only: a huge one is refused
// before it is decoded, and the main server never sees it.
func TestAnonymousCallersCannotSendBigMessages(t *testing.T) {
	s := startServer(t, nil)
	ctx, cancel := bg()
	defer cancel()
	_, err := s.dial(t).Enroll(ctx, &protobuf.EnrollRequest{Token: strings.Repeat("x", 1<<20)})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a 1 MiB Enroll without a certificate: %v", err)
	}
	// a normal request still gets to the handler
	if _, err := s.dial(t).Enroll(ctx, &protobuf.EnrollRequest{Token: "wrong"}); status.Code(err) == codes.ResourceExhausted || status.Code(err) == codes.Unavailable {
		t.Fatalf("a small Enroll: %v", err)
	}
}

// A-4: the authenticated server reads at most 4 MiB per message: a certificate holder cannot make a replica buffer tens of MiB.
func TestAuthenticatedServerReadsFourMiBAtMost(t *testing.T) {
	s := startServer(t, nil)
	ctx, cancel := bg()
	defer cancel()
	c := s.dial(t, issueFor(t, s.r, "acme"))
	// The Enroll call is refused on the main server after decoding (FailedPrecondition), so it shows whether the message was read.
	if _, err := c.Enroll(ctx, &protobuf.EnrollRequest{Token: strings.Repeat("x", 3<<20)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a 3 MiB message must be read: %v", err)
	}
	if _, err := c.Enroll(ctx, &protobuf.EnrollRequest{Token: strings.Repeat("x", 5<<20)}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a 5 MiB message must be refused before it is decoded: %v", err)
	}
}

// The Enroll server is rate limited, all callers together.
func TestEnrollIsRateLimited(t *testing.T) {
	s := startServer(t, func(c *config.Config) { c.Server.EnrollRate, c.Server.EnrollBurst = 0.01, 2 })
	ctx, cancel := bg()
	defer cancel()
	c := s.dial(t)
	limited := 0
	for i := 0; i < 6; i++ {
		if _, err := c.Enroll(ctx, &protobuf.EnrollRequest{Token: "wrong"}); status.Code(err) == codes.ResourceExhausted {
			limited++
		}
	}
	if limited < 3 {
		t.Fatalf("only %d of 6 calls were limited with a burst of 2", limited)
	}
}

// A holder of a certificate does not use the open door: Enroll is for a connection without one.
func TestEnrollOnTheMainServerIsRefused(t *testing.T) {
	s := startServer(t, nil)
	ctx, cancel := bg()
	defer cancel()
	cert := issueFor(t, s.r, "acme")
	if _, err := s.dial(t, cert).Enroll(ctx, &protobuf.EnrollRequest{Token: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Enroll with a client certificate: %v", err)
	}
}

// Connections without a certificate are capped in total (there is no per-address limit: the source address is shared behind the
// gateway); the rest are closed at once.
func TestAnonymousConnectionsAreCappedInTotal(t *testing.T) {
	s := startServer(t, func(c *config.Config) { c.Server.MaxAnonymousConns = 2 })
	var conns []*tls.Conn
	for i := 0; i < 4; i++ {
		c, err := tls.Dial("tcp", s.addr, s.tlsConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	open, closed := 0, 0
	for _, c := range conns {
		// A served connection stays open (the server may send its settings first); a refused one ends.
		_ = c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		var err error
		for err == nil {
			_, err = c.Read(make([]byte, 512))
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			open++
		} else {
			closed++
		}
	}
	if open != 2 || closed != 2 {
		t.Fatalf("open %d, closed %d: want 2 and 2", open, closed)
	}
}

// A client that does not speak h2 over TLS is not a gRPC client and is dropped.
func TestNonH2ConnectionsAreDropped(t *testing.T) {
	s := startServer(t, nil)
	cfg := s.tlsConfig()
	cfg.NextProtos = []string{"http/1.1"}
	c, err := tls.Dial("tcp", s.addr, cfg)
	if err != nil {
		return // refused during the handshake: fine too
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection must be closed")
	}
}

// There is one switch for running without client certificates, AGENT_ALLOW_INSECURE; without it mTLS and TLS are required, and a missing
// certificate is an error (no way to run plaintext by accident).
func TestInsecureModeIsOneSwitchAndTLSStaysOn(t *testing.T) {
	r := newEnrollRig(t)
	cfg := &config.Config{}
	if _, err := New(cfg, r.h); err == nil {
		t.Fatal("no certificates and no switch: the server must not start")
	}
	cfg.AllowInsecure = true
	if _, err := New(cfg, r.h); err == nil {
		t.Fatal("the switch turns the client check off, TLS needs its certificate still")
	}
	// With the switch and the server certificate the server starts (startServerWith fails the test otherwise).
	startServerWith(t, func(c *config.Config) { c.AllowInsecure = true }, nil)
}

// R-6: a stream is cut when its caller's authority ends.
func TestStreamIsCutWhenTheCallerIsNoLongerAdmitted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	revoked := make(chan struct{})
	admit := func(context.Context) error {
		select {
		case <-revoked:
			return status.Error(codes.PermissionDenied, "revoked")
		default:
			return nil
		}
	}
	w := watchStream(fakeStream{ctx: ctx}, 10*time.Millisecond, admit)
	select {
	case <-w.Context().Done():
		t.Fatal("cut while admitted")
	case <-time.After(60 * time.Millisecond):
	}
	close(revoked)
	select {
	case <-w.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the stream must be cut after the revocation")
	}
	if status.Code(w.failure()) != codes.PermissionDenied {
		t.Fatalf("the error the caller sees: %v", w.failure())
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

// slowPing answers Ping only when released.
type slowPing struct {
	protobuf.UnimplementedLabManagerServer
	started chan struct{}
	release chan struct{}
}

func (p *slowPing) Ping(context.Context, *protobuf.Empty) (*protobuf.Empty, error) {
	close(p.started)
	<-p.release
	return &protobuf.Empty{}, nil
}

// E-6: on SIGTERM the agent finishes the calls that are running (a CreateLabGroupClients that has made its key must answer with it) and takes no new ones.
func TestGracefulStopLetsARunningCallFinish(t *testing.T) {
	impl := &slowPing{started: make(chan struct{}), release: make(chan struct{})}
	s := startServerWith(t, nil, impl)
	cert := issueFor(t, s.r, "acme")
	client := s.dial(t, cert)
	ctx, cancel := bg()
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := client.Ping(ctx, &protobuf.Empty{}); result <- err }()
	<-impl.started

	stopped := make(chan struct{})
	go func() { s.srv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("GracefulStop returned while a call was running")
	case <-time.After(300 * time.Millisecond):
	}
	// new connections are refused during the shutdown
	if c, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
		_ = c.Close()
		t.Error("a new connection was accepted during the shutdown")
	}
	close(impl.release)
	if err := <-result; err != nil {
		t.Fatalf("the running call must finish: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("GracefulStop did not return after the call finished")
	}
}
