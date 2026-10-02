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

// Enrolling again moves the epoch: every earlier certificate stops working.
func TestEnrollingAgainRevokesTheEarlierCertificates(t *testing.T) {
	r := newEnrollRig(t)
	pub, _ := accessKey(t)
	first, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(first.CertificatePem))
	firstCert, _ := x509.ParseCertificate(block.Bytes)
	ctx1 := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{firstCert}}}})
	if err := r.h.Authorize(ctx1); err != nil {
		t.Fatalf("a fresh certificate works: %v", err)
	}

	// the admin issues a new token; ten minutes later the tenant enrolls again
	exp := metav1.NewTime(r.now.Add(time.Hour))
	ten, _ := r.h.cs.LaboratoryV1alpha1().Tenants().Get(context.Background(), "acme", metav1.GetOptions{})
	ten.Status.Enrollment = &laboratoryv1alpha1.TenantEnrollment{TokenHash: hashToken("second-token"), ExpiresAt: &exp}
	if _, err := r.h.cs.LaboratoryV1alpha1().Tenants().UpdateStatus(context.Background(), ten, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.h.clock = func() time.Time { return r.now.Add(10 * time.Minute) }
	if _, err := r.enroll(t, "second-token", csrPEM(t, newECKey(t), "x"), pub, "k2"); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Authorize(ctx1); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the first certificate must be revoked by the second enrollment: %v", err)
	}
	// a certificate renewed (issued) after the new epoch is fine
	if err := r.h.Authorize(callAs("acme", r.now.Add(11*time.Minute))); err != nil {
		t.Fatalf("a certificate issued after the epoch works: %v", err)
	}
	// and a revoked certificate cannot be used to renew
	if _, err := r.h.RenewCertificate(ctx1, nil); err == nil {
		t.Fatal("renewing with a revoked certificate must not work")
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

// The token is burnt before the certificate is signed: when signing fails the token is spent, never reusable.
func TestTokenIsBurntBeforeSigning(t *testing.T) {
	r := newEnrollRig(t)
	r.h.SetClientCA("/nonexistent.crt", "/nonexistent.key", 0) // signing will fail
	pub, _ := accessKey(t)
	if _, err := r.enroll(t, r.token, csrPEM(t, newECKey(t), "x"), pub, "k1"); err == nil {
		t.Fatal("signing must fail")
	}
	ten, _ := r.h.cs.LaboratoryV1alpha1().Tenants().Get(context.Background(), "acme", metav1.GetOptions{})
	if ten.Status.Enrollment.UsedAt == nil || ten.Status.CertificatesNotBefore == nil {
		t.Fatal("the token is burnt and the epoch moved even though signing failed")
	}
}
