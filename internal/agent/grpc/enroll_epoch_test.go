package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// callAs is a context carrying a client certificate for cn issued at the given moment (as issueCertificate sets it:
// NotBefore is certBackdate before the moment of issue).
func callAs(cn string, issued time.Time) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}, NotBefore: issued.Add(-certBackdate)}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})
}

func tenantCreatedAt(name string, created time.Time) *laboratoryv1alpha1.Tenant {
	t := newTenantTenant(name, false, nil)
	t.CreationTimestamp = metav1.NewTime(created)
	return t
}

// The audit's case: a tenant is deleted and created again under the same name; the certificates of the old one no longer
// work, while a certificate issued after the new tenant exists does.
func TestCertificateOfADeletedTenantFailsWhenTheNameIsReused(t *testing.T) {
	t0 := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	old := callAs("acme", t0.Add(time.Hour)) // issued while the first tenant existed
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{tenantCreatedAt("acme", t0)})
	if err := h.Authorize(old); err != nil {
		t.Fatalf("the old certificate works for the old tenant: %v", err)
	}
	// the tenant is deleted and created again a day later
	t1 := t0.Add(24 * time.Hour)
	h2 := tenantHandler(t, []*laboratoryv1alpha1.Tenant{tenantCreatedAt("acme", t1)})
	if err := h2.Authorize(old); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the old certificate must fail for the new tenant: %v", err)
	}
	if err := h2.Authorize(callAs("acme", t1.Add(time.Minute))); err != nil {
		t.Fatalf("a certificate issued for the new tenant works: %v", err)
	}
}

func parseCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func asCert(c *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}}})
}

// issueNextToken gives the tenant a new unused token, as the operator does on a request of the admin.
func (r *enrollRig) issueNextToken(t *testing.T, token string) {
	t.Helper()
	exp := metav1.NewTime(r.now.Add(time.Hour))
	tenants := r.h.cs.LaboratoryV1alpha1().Tenants()
	ten, err := tenants.Get(context.Background(), "acme", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ten.Labels = map[string]string{names.LabelEnrollmentToken: names.EnrollmentTokenLabel(hashToken(token))}
	if ten, err = tenants.Update(context.Background(), ten, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ten.Status.Enrollment = &laboratoryv1alpha1.TenantEnrollment{TokenHash: hashToken(token), ExpiresAt: &exp}
	if _, err := tenants.UpdateStatus(context.Background(), ten, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Enrolling again moves the epoch: every earlier certificate stops working.
func TestEnrollingAgainRevokesTheEarlierCertificates(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	first, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1")
	if err != nil {
		t.Fatal(err)
	}
	ctx1 := asCert(parseCert(t, first.CertificatePem))
	if err := r.h.Authorize(ctx1); err != nil {
		t.Fatalf("a fresh certificate works: %v", err)
	}

	// the admin issues a new token; ten minutes later the tenant enrolls again
	r.issueNextToken(t, "second-token")
	r.h.clock = func() time.Time { return r.now.Add(10 * time.Minute) }
	second, err := r.enroll(t, "second-token", csrPEM(t, newECKey(t), "x"), pub, "k2")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.h.Authorize(ctx1); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the first certificate must be revoked by the second enrollment: %v", err)
	}
	if err := r.h.Authorize(asCert(parseCert(t, second.CertificatePem))); err != nil {
		t.Fatalf("a certificate issued in the new epoch works: %v", err)
	}
	// and a revoked certificate cannot be used to renew
	if _, err := r.h.RenewCertificate(ctx1, &protobuf.RenewCertificateRequest{CsrPem: csrPEM(t, newECKey(t), "x")}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("renewing with a revoked certificate: %v", err)
	}
}

// The audit's one-second hole: enrolling again within the same second as the first used to leave the first certificate valid
// (the comparison was on whole seconds). The epoch is a number now, so the clock does not matter.
func TestEnrollingTwiceInTheSameSecondRevokesTheFirst(t *testing.T) {
	r := newEnrollRig(t) // the clock is fixed: both enrollments happen at the same instant
	pub, _ := accessKey(t)
	first, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1")
	if err != nil {
		t.Fatal(err)
	}
	r.issueNextToken(t, "second-token")
	second, err := r.enroll(t, "second-token", csrPEM(t, newECKey(t), "x"), pub, "k2")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.h.Authorize(asCert(parseCert(t, first.CertificatePem))); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the first certificate survived the second enrollment of the same second: %v", err)
	}
	if err := r.h.Authorize(asCert(parseCert(t, second.CertificatePem))); err != nil {
		t.Fatal(err)
	}
}

// A renewal loop cannot carry a revoked certificate across the revocation: renewal is rate limited, and what it issues is in the
// epoch it read, so a certificate renewed just before an enrollment dies with it.
func TestRenewalIsRateLimitedAndStaysInItsEpoch(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	first, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := asCert(parseCert(t, first.CertificatePem))
	renew := func() (*protobuf.CertificateResponse, error) {
		return r.h.RenewCertificate(ctx, &protobuf.RenewCertificateRequest{CsrPem: csrPEM(t, newECKey(t), "x")})
	}
	renewed, err := renew()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renew(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a second renewal at once: %v", err)
	}
	r.h.clock = func() time.Time { return r.now.Add(DefaultRenewMinInterval + time.Second) }
	if _, err := renew(); err != nil {
		t.Fatalf("a renewal after the interval: %v", err)
	}
	// the renewed certificate is in the same epoch; an enrollment kills both
	r.issueNextToken(t, "second-token")
	if _, err := r.enroll(t, "second-token", csrPEM(t, newECKey(t), "x"), pub, "k2"); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Authorize(asCert(parseCert(t, renewed.CertificatePem))); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a renewed certificate must not outlive the enrollment: %v", err)
	}
}

func claimCert(cn string, epoch int64, uid string) *x509.Certificate {
	ext, _ := epochExtension(epoch, uid)
	return &x509.Certificate{Subject: pkix.Name{CommonName: cn}, Extensions: []pkix.Extension{ext}}
}

// The epoch is compared exactly, with the Tenant's UID.
func TestEpochClaimIsExact(t *testing.T) {
	ten := newTenantTenant("acme", false, nil)
	ten.UID = "uid-1"
	ten.Status.CertificateEpoch = 3
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{ten})
	for name, c := range map[string]struct {
		cert *x509.Certificate
		ok   bool
	}{
		"current":        {claimCert("acme", 3, "uid-1"), true},
		"older epoch":    {claimCert("acme", 2, "uid-1"), false},
		"newer epoch":    {claimCert("acme", 4, "uid-1"), false},
		"another tenant": {claimCert("acme", 3, "uid-2"), false},
		"legacy (no claim) after the first enrollment": {&x509.Certificate{Subject: pkix.Name{CommonName: "acme"}, NotBefore: time.Now().Add(time.Hour)}, false},
	} {
		err := h.Authorize(asCert(c.cert))
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a malformed claim is a refusal, never "no claim"
	bad := &x509.Certificate{Subject: pkix.Name{CommonName: "acme"}, Extensions: []pkix.Extension{{Id: oidTenantEpoch, Value: []byte{1, 2, 3}}}}
	if err := h.Authorize(asCert(bad)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("malformed claim: %v", err)
	}
}

// Before any enrollment of this version a certificate with no claim is judged by time, so what exists keeps working.
func TestLegacyCertificatesKeepWorkingUntilTheFirstEnrollment(t *testing.T) {
	t0 := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{tenantCreatedAt("acme", t0)})
	if err := h.Authorize(callAs("acme", t0.Add(time.Hour))); err != nil {
		t.Fatalf("an existing certificate: %v", err)
	}
}

// The default tenant is revoked like any other: its epoch is checked.
func TestDefaultTenantCertificatesAreRevocable(t *testing.T) {
	ten := newTenantTenant("default", false, nil)
	ten.UID = "uid-d"
	ten.Status.CertificateEpoch = 2
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{ten})
	if err := h.Authorize(asCert(claimCert("default", 1, "uid-d"))); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an old certificate of the default tenant: %v", err)
	}
	if err := h.Authorize(asCert(claimCert("default", 2, "uid-d"))); err != nil {
		t.Fatalf("the current one: %v", err)
	}
}

// An enrollment replaces the access keys: a key added by a holder of the revoked certificate does not survive it.
func TestEnrollmentReplacesTheAccessKeys(t *testing.T) {
	r := newEnrollRig(t)
	pub1, _ := accessKey(t)
	first, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub1, "k1")
	if err != nil {
		t.Fatal(err)
	}
	// the holder of the (soon revoked) certificate adds ten keys
	ctx := asCert(parseCert(t, first.CertificatePem))
	_ = ctx
	for i := 0; i < 9; i++ {
		k, _ := accessKey(t)
		if err := r.h.putAccessKey(context.Background(), "acme", "stolen"+string(rune('a'+i)), mustParseKey(t, k)); err != nil {
			t.Fatal(err)
		}
	}
	// the rightful owner enrolls again (every slot is full) and ends with exactly its own key
	r.issueNextToken(t, "second-token")
	pub2, _ := accessKey(t)
	if _, err := r.enroll(t, "second-token", csrPEM(t, newECKey(t), "x"), pub2, "k2"); err != nil {
		t.Fatalf("a tenant whose slots were filled can enroll again: %v", err)
	}
	s, err := r.h.k8s.CoreV1().Secrets(names.TenantsNamespace).Get(context.Background(), "tenant-acme-access-keys", metav1.GetOptions{})
	if err != nil || len(s.Data) != 1 || len(s.Data["k2"]) == 0 {
		t.Fatalf("keys after the second enrollment: %v %v", sortedKeyIDs(s.Data), err)
	}
}

func mustParseKey(t *testing.T, pemStr string) []byte {
	t.Helper()
	k, err := parseAccessKey(pemStr)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// L-2: what can be known to fail does not cost the token.
func TestNothingIsBurntForARequestBoundToFail(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	for _, id := range []string{".", ".."} {
		if _, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, id); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("key id %q: %v", id, err)
		}
	}
	r.h.SetClientCA("/nonexistent.crt", "/nonexistent.key", 0) // no way to sign
	if _, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1"); err == nil {
		t.Fatal("signing must fail")
	}
	ten, _ := r.h.cs.LaboratoryV1alpha1().Tenants().Get(context.Background(), "acme", metav1.GetOptions{})
	if ten.Status.Enrollment.UsedAt != nil || ten.Status.CertificateEpoch != 0 {
		t.Fatal("the token must still be usable and the epoch unmoved")
	}
}

// The token is found by its label: a request with an unknown token reads no Tenant list beyond the selector's.
func TestTokenIsFoundByLabelOnly(t *testing.T) {
	r := newEnrollRig(t)
	ten, _ := r.h.cs.LaboratoryV1alpha1().Tenants().Get(context.Background(), "acme", metav1.GetOptions{})
	ten.Labels = nil // the operator has not labelled it
	if _, err := r.h.cs.LaboratoryV1alpha1().Tenants().Update(context.Background(), ten, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pub, _ := accessKey(t)
	if _, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a Tenant without the token label is not found: %v", err)
	}
}

// L4: of many concurrent requests with the same unused token exactly one gets a certificate.
func TestOneTokenGivesOneCertificateUnderConcurrency(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for i := 0; i < n; i++ {
		csr := csrPEM(t, newECKey(t), "x")
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.enroll(t, r.token, csr, pub, "k1")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
			} else {
				lost++
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != n-1 {
		t.Fatalf("%d certificates issued for one token (and %d refused), want exactly 1", won, lost)
	}
}
