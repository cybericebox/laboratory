package l7

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

const grpcHostLabel = "bot-k7xg"
const grpcHost = grpcHostLabel + ".challenges.example.com"

// grpcRig is a proxy in front of a fake device that serves gRPC over h2c, with a lab group, a client and a policy in a fake cluster.
type grpcRig struct {
	t       *testing.T
	h       *Handler
	proxy   *httptest.Server
	cluster client.Client
	policy  *lab.LabGroupAccessPolicy
	priv    ed25519.PrivateKey
	keyOK   atomic.Bool
	device  *grpc.Server

	mu       sync.Mutex
	seenMD   metadata.MD
	seenPeer string
}

func newGRPCRig(t *testing.T) *grpcRig {
	t.Helper()
	rig := &grpcRig{t: t}
	rig.keyOK.Store(true)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	rig.priv = priv

	// the fake device: unary, server+client streaming (echo) and a call that sets headers and trailers
	rig.device = grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		md, _ := metadata.FromIncomingContext(stream.Context())
		rig.mu.Lock()
		rig.seenMD = md
		rig.mu.Unlock()
		switch method {
		case "/test.Echo/Unary":
			var in wrapperspb.StringValue
			if err := stream.RecvMsg(&in); err != nil {
				return err
			}
			stream.SetTrailer(metadata.Pairs("x-device-trailer", "done"))
			return stream.SendMsg(wrapperspb.String("device:" + in.Value))
		case "/test.Echo/Bidi":
			for {
				var in wrapperspb.StringValue
				if err := stream.RecvMsg(&in); err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}
				if err := stream.SendMsg(wrapperspb.String("echo:" + in.Value)); err != nil {
					return err
				}
			}
		}
		return status.Error(codes.Unimplemented, "no such method")
	}))
	dl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = rig.device.Serve(dl) }()
	t.Cleanup(rig.device.Stop)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "bot-1", Namespace: "legacy"}}
	rig.policy = &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "legacy"},
		Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: grpcHostLabel, Namespace: "legacy",
		Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "bot", names.LabelAccessKind: "grpc"}},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Name: "grpc", Port: 9099, TargetPort: intstr.FromInt32(9099), Protocol: corev1.ProtocolTCP}}}}
	rig.cluster = fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, member, rig.policy, svc,
		&lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}).Build()

	keys := func(tenant, kid string) (ed25519.PublicKey, bool) {
		if tenant == "acme" && kid == "k1" && rig.keyOK.Load() {
			return pub, true
		}
		return nil, false
	}
	transport := newGRPCTransport()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "bot-k7xg.legacy.svc.cluster.local:9099" {
			return nil, io.ErrClosedPipe
		}
		return (&net.Dialer{}).DialContext(ctx, network, dl.Addr().String())
	}
	rig.h = NewHandler(keys, testSecret, "challenges.example.com", "challenge", nil).
		WithAccessReader(&AccessReader{Reader: rig.cluster}).WithGRPCTransport(transport)
	rig.proxy = httptest.NewUnstartedServer(rig.h)
	rig.proxy.EnableHTTP2 = true
	rig.proxy.Config.ReadTimeout = 300 * time.Millisecond // the server's own limits must not cut a stream of this kind
	rig.proxy.StartTLS()
	t.Cleanup(rig.proxy.Close)
	return rig
}

func (r *grpcRig) token(mutate func(*grpcClaims)) string {
	r.t.Helper()
	now := time.Now()
	c := grpcClaims{GroupID: "g1", Host: grpcHostLabel, Run: "run-1", RegisteredClaims: jwt.RegisteredClaims{
		Issuer: "acme", Subject: "bot-1", Audience: jwt.ClaimStrings{HandoffAudience},
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
	}}
	if mutate != nil {
		mutate(&c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(r.priv)
	if err != nil {
		r.t.Fatal(err)
	}
	return signed
}

func (r *grpcRig) dial(authority string) *grpc.ClientConn {
	r.t.Helper()
	conn, err := grpc.NewClient(r.proxy.Listener.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})),
		grpc.WithAuthority(authority))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func TestGRPCUnaryThroughTheProxyKeepsTrailersAndHidesTheToken(t *testing.T) {
	rig := newGRPCRig(t)
	conn := rig.dial(grpcHost)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out wrapperspb.StringValue
	var trailer metadata.MD
	err := conn.Invoke(withToken(ctx, rig.token(nil)), "/test.Echo/Unary", wrapperspb.String("hi"), &out, grpc.Trailer(&trailer))
	if err != nil || out.Value != "device:hi" {
		t.Fatalf("unary: %q %v", out.Value, err)
	}
	if got := trailer.Get("x-device-trailer"); len(got) != 1 || got[0] != "done" {
		t.Fatalf("the trailer of the device did not reach the client: %v", trailer)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.seenMD.Get("authorization")) != 0 || len(rig.seenMD.Get("cookie")) != 0 {
		t.Fatalf("the device saw the credentials of the client: %v", rig.seenMD)
	}
}

// A bidi stream lives longer than the server's read timeout (300 ms here) and passes messages both ways while it does.
func TestGRPCBidiStreamIsNotCutByTheServerTimeouts(t *testing.T) {
	rig := newGRPCRig(t)
	conn := rig.dial(grpcHost)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := conn.NewStream(withToken(ctx, rig.token(nil)), &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Echo/Bidi")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(900 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		if err := stream.SendMsg(wrapperspb.String("m")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		var out wrapperspb.StringValue
		if err := stream.RecvMsg(&out); err != nil || out.Value != "echo:m" {
			t.Fatalf("recv %d: %q %v", i, out.Value, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var out wrapperspb.StringValue
	if err := stream.RecvMsg(&out); err != io.EOF {
		t.Fatalf("the end of the stream: %v", err)
	}
}

func TestGRPCRefusalsAreGRPCStatuses(t *testing.T) {
	rig := newGRPCRig(t)
	conn := rig.dial(grpcHost)
	other := rig.dial("web-k7xg.challenges.example.com") // not a gRPC host: the web path answers
	call := func(c *grpc.ClientConn, md string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if md != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", md)
		}
		var out wrapperspb.StringValue
		return c.Invoke(ctx, "/test.Echo/Unary", wrapperspb.String("x"), &out)
	}
	cases := []struct {
		name string
		auth string
		want codes.Code
	}{
		{"no token", "", codes.Unauthenticated},
		{"not a bearer", "Basic abc", codes.Unauthenticated},
		{"garbage", "Bearer not.a.jwt", codes.Unauthenticated},
		{"expired", "Bearer " + rig.token(func(c *grpcClaims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-3 * time.Hour))
			c.NotBefore = c.IssuedAt
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
		}), codes.Unauthenticated},
		{"another host", "Bearer " + rig.token(func(c *grpcClaims) { c.Host = "bot-zzzzg" }), codes.Unauthenticated},
		{"no run", "Bearer " + rig.token(func(c *grpcClaims) { c.Run = "" }), codes.Unauthenticated},
		{"lives past the hard cap", "Bearer " + rig.token(func(c *grpcClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(25 * time.Hour)) }), codes.Unauthenticated},
		{"another tenant's group", "Bearer " + rig.token(func(c *grpcClaims) { c.GroupID = "g-other" }), codes.Unauthenticated},
		{"a client the group does not have", "Bearer " + rig.token(func(c *grpcClaims) { c.Subject = "ghost" }), codes.NotFound},
	}
	for _, c := range cases {
		err := call(conn, c.auth)
		if got := status.Code(err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		if strings.Contains(err.Error(), "<html") {
			t.Errorf("%s: an HTML answer: %v", c.name, err)
		}
	}
	// a handoff-style token (no run) and a cookie are no way in
	if err := call(other, "Bearer "+rig.token(nil)); status.Code(err) == codes.OK {
		t.Errorf("a token for the gRPC host worked on a web host: %v", err)
	}
}

// The key that signed the token goes away, or the policy stops allowing the client: the stream is cut at the next live check.
func TestGRPCStreamIsCancelledWhenAccessIsLost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(*grpcRig)
	}{
		{"the policy no longer allows the client", func(r *grpcRig) {
			if err := r.cluster.Delete(context.Background(), r.policy); err != nil {
				r.t.Fatal(err)
			}
		}},
		{"the access key is removed", func(r *grpcRig) { r.keyOK.Store(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGRPCRig(t)
			conn := rig.dial(grpcHost)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := conn.NewStream(withToken(ctx, rig.token(nil)), &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Echo/Bidi")
			if err != nil {
				t.Fatal(err)
			}
			var out wrapperspb.StringValue
			if err := stream.SendMsg(wrapperspb.String("a")); err != nil {
				t.Fatal(err)
			}
			if err := stream.RecvMsg(&out); err != nil {
				t.Fatal(err)
			}
			if n := rig.h.CheckLive(); n != 0 {
				t.Fatalf("a call with access was closed (%d)", n)
			}
			tc.revoke(rig)
			if n := rig.h.CheckLive(); n != 1 {
				t.Fatalf("CheckLive closed %d calls, want 1", n)
			}
			if err := stream.RecvMsg(&out); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("the stream did not end with PermissionDenied after the access was lost: %v", err)
			}
			// and a new call is refused
			var o2 wrapperspb.StringValue
			if err := conn.Invoke(withToken(ctx, rig.token(nil)), "/test.Echo/Unary", wrapperspb.String("x"), &o2); err == nil {
				t.Fatal("a new call went through")
			}
		})
	}
}

// The token ends every call made with it: the stream stops at exp even when nothing else changed.
func TestGRPCStreamEndsAtTheExpiryOfTheToken(t *testing.T) {
	rig := newGRPCRig(t)
	conn := rig.dial(grpcHost)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token := rig.token(nil)
	stream, err := conn.NewStream(withToken(ctx, token), &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Echo/Bidi")
	if err != nil {
		t.Fatal(err)
	}
	var out wrapperspb.StringValue
	_ = stream.SendMsg(wrapperspb.String("a"))
	if err := stream.RecvMsg(&out); err != nil {
		t.Fatal(err)
	}
	rig.h.now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	if n := rig.h.CheckLive(); n != 1 {
		t.Fatalf("CheckLive closed %d, want 1", n)
	}
	if err := stream.RecvMsg(&out); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("the stream did not end with Unauthenticated at the expiry of the token: %v", err)
	}
}

// Over HTTP/1.1, or without a gRPC content type, the host has nothing to offer and says so in plain text.
func TestGRPCHostIsNotAWebHost(t *testing.T) {
	rig := newGRPCRig(t)
	req := httptest.NewRequest("GET", "https://"+grpcHost+"/", nil)
	req.Host = grpcHost
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestGRPCTokenLifetimeCapIsConfigurable(t *testing.T) {
	rig := newGRPCRig(t)
	rig.h.WithGRPCTokenMaxLifetime(time.Hour)
	conn := rig.dial(grpcHost)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out wrapperspb.StringValue
	err := conn.Invoke(withToken(ctx, rig.token(nil)), "/test.Echo/Unary", wrapperspb.String("x"), &out) // lives 2 h
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a token past the configured cap: %v", err)
	}
}
