package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

type enrollRig struct {
	h      *Handler
	ca     *x509.Certificate
	now    time.Time
	token  string
	tenant string
}

func newEnrollRig(t *testing.T) *enrollRig {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "laboratory-agent-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca, _ := x509.ParseCertificate(der)
	kder, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)

	token := "the-one-time-token"
	exp := metav1.NewTime(now.Add(time.Hour))
	ten := newTenantTenant("acme", false, nil)
	ten.UID = "uid-acme"
	ten.Labels = map[string]string{names.LabelEnrollmentToken: names.EnrollmentTokenLabel(hashToken(token))}
	ten.Status.Enrollment = &laboratoryv1alpha1.TenantEnrollment{TokenHash: hashToken(token), ExpiresAt: &exp}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{ten})
	h.SetClientCA(certFile, keyFile, 0)
	h.clock = func() time.Time { return now }
	return &enrollRig{h: h, ca: ca, now: now, token: token, tenant: "acme"}
}

func csrPEM(t *testing.T, key any, cn string) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn, Organization: []string{"evil"}}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func accessKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), priv
}

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (r *enrollRig) enroll(t *testing.T, token, csr, keyPEM, id string) (*protobuf.CertificateResponse, error) {
	t.Helper()
	return r.h.Enroll(context.Background(), &protobuf.EnrollRequest{Token: token, CsrPem: csr, AccessPublicKeyPem: keyPEM, AccessKeyId: id})
}

func TestEnrollIssuesACertificateAndStoresTheAccessKey(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	key := newECKey(t)
	resp, err := r.enroll(t, r.token, csrPEM(t, key, "someone-else"), pub, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.NotAfterUnix != r.now.Add(DefaultClientCertTTL).Unix() {
		t.Fatalf("%+v", resp)
	}
	block, _ := pem.Decode([]byte(resp.CertificatePem))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// The requested subject is ignored: the CN is the tenant, nothing else.
	if cert.Subject.CommonName != "acme" || len(cert.Subject.Organization) != 0 {
		t.Fatalf("subject %v", cert.Subject)
	}
	pool := x509.NewCertPool()
	pool.AddCert(r.ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: r.now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("the certificate must verify against the client CA: %v", err)
	}
	if got := cert.PublicKey.(*ecdsa.PublicKey); !got.Equal(&key.PublicKey) {
		t.Fatal("the certificate is for the key of the request")
	}
	if !strings.Contains(resp.ChainPem, "BEGIN CERTIFICATE") {
		t.Fatal("chain")
	}
	// The access key is stored under the fixed name.
	s, err := r.h.k8s.CoreV1().Secrets(names.AccessKeysNamespace).Get(context.Background(), "tenant-acme-access-keys", metav1.GetOptions{})
	if err != nil || len(s.Data["k1"]) == 0 {
		t.Fatalf("access keys secret: %v %v", s, err)
	}
	// The token is burnt.
	ten, _ := r.h.cs.LaboratoryV1alpha1().Tenants().Get(context.Background(), "acme", metav1.GetOptions{})
	if ten.Status.Enrollment.UsedAt == nil {
		t.Fatal("the token must be marked used")
	}
	if _, err := r.enroll(t, r.token, csrPEM(t, key, "x"), pub, "k2"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("re-enrolling a burnt token: %v", err)
	}
}

func TestEnrollRefusesBadTokens(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	csr := csrPEM(t, newECKey(t), "x")
	for name, tok := range map[string]string{"unknown": "nope", "empty": ""} {
		if _, err := r.enroll(t, tok, csr, pub, "k"); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Expired.
	r.h.clock = func() time.Time { return r.now.Add(2 * time.Hour) }
	if _, err := r.enroll(t, r.token, csr, pub, "k"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expired: %v", err)
	}
	// An expired or unknown token is told apart from nothing: the same answer, no oracle.
	_, e1 := r.enroll(t, "nope", csr, pub, "k")
	_, e2 := r.enroll(t, r.token, csr, pub, "k")
	if e1.Error() != e2.Error() {
		t.Fatalf("the answers differ: %v / %v", e1, e2)
	}
	if _, err := r.h.k8s.CoreV1().Secrets(names.AccessKeysNamespace).Get(context.Background(), "tenant-acme-access-keys", metav1.GetOptions{}); err == nil {
		t.Fatal("a refused enrollment stores nothing")
	}
}

func TestEnrollChecksTheRequest(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	weakRSA, _ := rsa.GenerateKey(rand.Reader, 1024)
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	p224, _ := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	good := csrPEM(t, newECKey(t), "x")
	tampered := strings.Replace(good, good[60:70], strings.ToUpper(good[60:70]), 1)
	cases := map[string]string{
		"garbage":      "not a pem",
		"weak RSA":     csrPEM(t, weakRSA, "x"),
		"ed25519":      csrPEM(t, edPriv, "x"),
		"EC below 256": csrPEM(t, p224, "x"),
		"tampered":     tampered,
	}
	for name, csr := range cases {
		if _, err := r.enroll(t, r.token, csr, pub, "k"); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	rsaPub, _ := x509.MarshalPKIXPublicKey(&weakRSA.PublicKey)
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rsaPub}))
	for name, c := range map[string][2]string{"rsa access key": {rsaPEM, "k"}, "bad id": {pub, "has space"}, "empty id": {pub, ""}, "no key": {"", "k"}} {
		if _, err := r.enroll(t, r.token, good, c[0], c[1]); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A good RSA-2048 request is accepted, and none of the refusals burnt the token.
	big, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := r.enroll(t, r.token, csrPEM(t, big, "x"), pub, "k"); err != nil {
		t.Fatalf("RSA 2048: %v", err)
	}
}

func TestRenewKeepsTheCN(t *testing.T) {
	r := newEnrollRig(t)
	resp, err := r.h.RenewCertificate(asClient("acme"), &protobuf.RenewCertificateRequest{CsrPem: csrPEM(t, newECKey(t), "other")})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(resp.CertificatePem))
	cert, _ := x509.ParseCertificate(block.Bytes)
	if cert.Subject.CommonName != "acme" {
		t.Fatalf("the renewed certificate is for the caller's tenant: %v", cert.Subject)
	}
	if _, err := r.h.RenewCertificate(context.Background(), &protobuf.RenewCertificateRequest{CsrPem: csrPEM(t, newECKey(t), "x")}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no certificate: %v", err)
	}
	if _, err := r.h.RenewCertificate(asClient("acme"), &protobuf.RenewCertificateRequest{CsrPem: "junk"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad csr: %v", err)
	}
	// The CA is the limit of a certificate's life.
	r.h.SetClientCA(r.h.caCertFile, r.h.caKeyFile, 10*365*24*time.Hour)
	r.h.clock = func() time.Time { return r.now.Add(time.Minute) } // a renewal is rate limited
	long, err := r.h.RenewCertificate(asClient("acme"), &protobuf.RenewCertificateRequest{CsrPem: csrPEM(t, newECKey(t), "x")})
	if err != nil {
		t.Fatal(err)
	}
	if long.NotAfterUnix != r.ca.NotAfter.Unix() {
		t.Fatalf("a certificate outlives its CA: %d vs %d", long.NotAfterUnix, r.ca.NotAfter.Unix())
	}
}

func TestRotateAndRemoveAccessKeys(t *testing.T) {
	r := newEnrollRig(t)
	ctx := asClient("acme")
	k1, _ := accessKey(t)
	k2, _ := accessKey(t)
	if _, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), k1, "old"); err != nil {
		t.Fatal(err)
	}
	ids := func() []string {
		s, _ := r.h.k8s.CoreV1().Secrets(names.AccessKeysNamespace).Get(context.Background(), "tenant-acme-access-keys", metav1.GetOptions{})
		return sortedKeyIDs(s.Data)
	}
	rot := func(key, id string) error {
		_, err := r.h.RotateAccessKey(ctx, &protobuf.RotateAccessKeyRequest{PublicKeyPem: key, KeyId: id})
		return err
	}
	// Overlap: both keys exist after the rotation.
	if err := rot(k2, "new"); err != nil || strings.Join(ids(), ",") != "new,old" {
		t.Fatalf("%v %v", err, ids())
	}
	if err := rot(k2, "new"); err != nil {
		t.Fatalf("the same key again is accepted: %v", err)
	}
	if err := rot(k1, "new"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("another key under a used id: %v", err)
	}
	if err := rot("junk", "x"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("junk: %v", err)
	}
	rm := func(id string) error {
		_, err := r.h.RemoveAccessKey(ctx, &protobuf.RemoveAccessKeyRequest{KeyId: id})
		return err
	}
	if err := rm("ghost"); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown id: %v", err)
	}
	if err := rm("old"); err != nil || strings.Join(ids(), ",") != "new" {
		t.Fatalf("%v %v", err, ids())
	}
	if err := rm("new"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("the last key stays: %v", err)
	}
	// A tenant keeps a bounded number of keys.
	for i := 0; i < maxAccessKeys-1; i++ {
		k, _ := accessKey(t)
		if err := rot(k, "k"+string(rune('a'+i))); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	extra, _ := accessKey(t)
	if err := rot(extra, "too-many"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("limit: %v", err)
	}
}

func TestEnrollWorksWithoutAClientCertificateOnTheServer(t *testing.T) {
	r := newEnrollRig(t)
	if _, err := r.h.Enroll(context.Background(), &protobuf.EnrollRequest{}); status.Code(err) == codes.Unauthenticated {
		t.Fatal("Enroll authenticates by token, not by certificate")
	}
}

// The handshake and the interceptors together: Enroll without a client certificate, every other
// call needs one, and a certificate issued by Enroll opens them.
func TestServerAdmitsEnrollWithoutACertificateAndNothingElse(t *testing.T) {
	r := newEnrollRig(t)
	dir := t.TempDir()
	caDER, _ := pem.Decode(mustRead(t, r.h.caCertFile))
	_ = caDER
	// A server certificate signed by the same CA.
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
	cfg.ServerTLS = config.ServerTLSConfig{Enabled: true, CertFile: srvCrt, KeyFile: srvKey}
	cfg.MTLS = config.MTLSConfig{Enabled: true, ClientCAFile: r.h.caCertFile}
	// Real time is needed for the handshake: the certificates were made around a fixed date.
	r.h.clock = nil
	srv, err := New(cfg, r.h)
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
	dial := func(certs ...tls.Certificate) protobuf.LabManagerClient {
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, Certificates: certs, ServerName: "localhost", Time: func() time.Time { return r.now }})))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return protobuf.NewLabManagerClient(conn)
	}
	anon := dial()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := anon.Ping(ctx, &protobuf.Empty{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("Ping without a certificate: the anonymous server knows Enroll only: %v", err)
	}
	if _, err := anon.Enroll(ctx, &protobuf.EnrollRequest{Token: "wrong"}); status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.Unavailable {
		t.Fatalf("Enroll without a certificate must reach the handler: %v", err)
	}
	// Enroll for real over the plain TLS connection (the handler's clock is fixed to the token's era).
	r.h.clock = func() time.Time { return r.now }
	pub, _ := accessKey(t)
	ckey := newECKey(t)
	resp, err := anon.Enroll(ctx, &protobuf.EnrollRequest{Token: r.token, CsrPem: csrPEM(t, ckey, "x"), AccessPublicKeyPem: pub, AccessKeyId: "k"})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// The certificate it issued opens the other calls.
	crt, err := tls.X509KeyPair([]byte(resp.CertificatePem), mustKeyPEM(t, ckey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(crt).Ping(ctx, &protobuf.Empty{}); err != nil {
		t.Fatalf("Ping with the enrolled certificate: %v", err)
	}
	// A certificate whose CN is no tenant is refused.
	stranger := issueFor(t, r, "stranger")
	if _, err := dial(stranger).Ping(ctx, &protobuf.Empty{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a stranger: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustKeyPEM(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// issueFor makes a client certificate with the given CN straight from the CA.
func issueFor(t *testing.T, r *enrollRig, cn string) tls.Certificate {
	t.Helper()
	key := newECKey(t)
	_, caKey, _, _ := loadCA(r.h.caCertFile, r.h.caKeyFile)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: cn}, NotBefore: r.now.Add(-time.Hour), NotAfter: r.now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, r.ca, &key.PublicKey, caKey)
	crt, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), mustKeyPEM(t, key))
	if err != nil {
		t.Fatal(err)
	}
	return crt
}
