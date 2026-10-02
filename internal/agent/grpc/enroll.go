package grpc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	// DefaultClientCertTTL is how long an issued client certificate is valid.
	DefaultClientCertTTL = 30 * 24 * time.Hour
	// maxAccessKeys is the most access keys one tenant keeps (a rotation needs two).
	maxAccessKeys = 10
	minRSABits    = 2048
)

var accessKeyIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// SetClientCA gives the agent the CA that signs client certificates (the one that verifies
// them on the server side) and the lifetime of what it signs. The files are read per
// signature, so a rotated CA needs no restart.
func (h *Handler) SetClientCA(certFile, keyFile string, ttl time.Duration) {
	h.caCertFile, h.caKeyFile, h.certTTL = certFile, keyFile, ttl
}

func denied() error {
	return status.Error(codes.PermissionDenied, "invalid, expired or already used enrollment token")
}

// Enroll exchanges a tenant's one-time token for a client certificate and registers the
// tenant's access public key. See the proto for the contract.
func (h *Handler) Enroll(ctx context.Context, in *protobuf.EnrollRequest) (*protobuf.CertificateResponse, error) {
	csr, err := parseCSR(in.GetCsrPem())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr_pem: %v", err)
	}
	keyPEM, err := parseAccessKey(in.GetAccessPublicKeyPem())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "access_public_key_pem: %v", err)
	}
	if !accessKeyIDRE.MatchString(in.GetAccessKeyId()) {
		return nil, status.Error(codes.InvalidArgument, "access_key_id: 1 to 64 characters of A-Z a-z 0-9 . _ -")
	}
	if in.GetToken() == "" {
		return nil, denied()
	}
	tenant, err := h.findTenantByToken(ctx, in.GetToken())
	if err != nil {
		return nil, err
	}
	// Burn first, then sign: of any number of requests that carry the same token, exactly one wins the conditional
	// write and gets a certificate. The burn also moves the enrollment epoch, which revokes every certificate issued
	// before it. A failure after the burn costs the token: the admin issues a new one.
	if err := h.burnToken(ctx, tenant.Name, in.GetToken()); err != nil {
		return nil, err
	}
	resp, err := h.issueCertificate(tenant.Name, csr.PublicKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "the token is used but the certificate was not issued (%v): ask the admin for a new token", err)
	}
	if err := h.putAccessKey(ctx, tenant.Name, in.GetAccessKeyId(), keyPEM); err != nil {
		return nil, status.Errorf(codes.Internal, "the token is used but the access key was not stored (%v): ask the admin for a new token", err)
	}
	return resp, nil
}

// RenewCertificate issues a new certificate for the caller's tenant.
func (h *Handler) RenewCertificate(ctx context.Context, in *protobuf.RenewCertificateRequest) (*protobuf.CertificateResponse, error) {
	if _, err := clientCN(ctx); err != nil {
		return nil, status.Error(codes.Unauthenticated, "a client certificate is required")
	}
	csr, err := parseCSR(in.GetCsrPem())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr_pem: %v", err)
	}
	return h.issueCertificate(tenantOf(ctx), csr.PublicKey)
}

// RotateAccessKey adds an access public key to the caller's tenant.
func (h *Handler) RotateAccessKey(ctx context.Context, in *protobuf.RotateAccessKeyRequest) (*protobuf.Empty, error) {
	keyPEM, err := parseAccessKey(in.GetPublicKeyPem())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "public_key_pem: %v", err)
	}
	if !accessKeyIDRE.MatchString(in.GetKeyId()) {
		return nil, status.Error(codes.InvalidArgument, "key_id: 1 to 64 characters of A-Z a-z 0-9 . _ -")
	}
	if err := h.putAccessKey(ctx, tenantOf(ctx), in.GetKeyId(), keyPEM); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}

// RemoveAccessKey removes a key of the caller's tenant; the last one stays.
func (h *Handler) RemoveAccessKey(ctx context.Context, in *protobuf.RemoveAccessKeyRequest) (*protobuf.Empty, error) {
	secrets := h.k8s.CoreV1().Secrets(names.TenantsNamespace)
	name := names.AccessKeysSecret(tenantOf(ctx))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		s, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return status.Errorf(codes.NotFound, "no access key %q", in.GetKeyId())
		}
		if err != nil {
			return err
		}
		if _, ok := s.Data[in.GetKeyId()]; !ok {
			return status.Errorf(codes.NotFound, "no access key %q", in.GetKeyId())
		}
		if len(s.Data) <= 1 {
			return status.Error(codes.FailedPrecondition, "the last access key cannot be removed: add the new one first")
		}
		delete(s.Data, in.GetKeyId())
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}

// findTenantByToken returns the tenant whose unexpired, unused token this is.
func (h *Handler) findTenantByToken(ctx context.Context, token string) (*laboratoryv1alpha1.Tenant, error) {
	want := hashToken(token)
	list, err := h.cs.LaboratoryV1alpha1().Tenants().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var found *laboratoryv1alpha1.Tenant
	for i := range list.Items {
		en := list.Items[i].Status.Enrollment
		if en == nil || en.TokenHash == "" {
			continue
		}
		// Compare every hash (no early exit on the first match's position).
		if subtle.ConstantTimeCompare([]byte(en.TokenHash), []byte(want)) == 1 {
			found = &list.Items[i]
		}
	}
	if found == nil || !tokenUsable(found.Status.Enrollment, h.now()) {
		return nil, denied()
	}
	return found, nil
}

func tokenUsable(en *laboratoryv1alpha1.TenantEnrollment, now time.Time) bool {
	return en != nil && en.UsedAt == nil && en.ExpiresAt != nil && now.Before(en.ExpiresAt.Time)
}

// burnToken marks the token used. It is a conditional write: of two clients with the same token
// only one succeeds.
func (h *Handler) burnToken(ctx context.Context, tenant, token string) error {
	tenants := h.cs.LaboratoryV1alpha1().Tenants()
	want := hashToken(token)
	for attempt := 0; attempt < 5; attempt++ {
		t, err := tenants.Get(ctx, tenant, metav1.GetOptions{})
		if err != nil {
			return denied()
		}
		en := t.Status.Enrollment
		if !tokenUsable(en, h.now()) || subtle.ConstantTimeCompare([]byte(en.TokenHash), []byte(want)) != 1 {
			return denied()
		}
		now := metav1.NewTime(h.now())
		en.UsedAt = &now
		t.Status.CertificatesNotBefore = &now
		if _, err = tenants.UpdateStatus(ctx, t, metav1.UpdateOptions{}); err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return denied()
}

func (h *Handler) now() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
}

// putAccessKey adds a key to the tenant's access-keys Secret. The same id with the same key is
// fine; the same id with another key is refused.
func (h *Handler) putAccessKey(ctx context.Context, tenant, id string, keyPEM []byte) error {
	secrets := h.k8s.CoreV1().Secrets(names.TenantsNamespace)
	name := names.AccessKeysSecret(tenant)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		s, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: names.TenantsNamespace, Labels: map[string]string{names.LabelPrefix + "tenant-name": tenant}},
				Data:       map[string][]byte{id: keyPEM},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if cur, ok := s.Data[id]; ok {
			if subtle.ConstantTimeCompare(cur, keyPEM) == 1 {
				return nil
			}
			return status.Errorf(codes.AlreadyExists, "access key %q exists with another key", id)
		}
		if len(s.Data) >= maxAccessKeys {
			return status.Errorf(codes.FailedPrecondition, "a tenant keeps at most %d access keys: remove an old one", maxAccessKeys)
		}
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data[id] = keyPEM
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		return err
	})
}

// certBackdate is how far a client certificate's NotBefore is set before the moment it was issued, for clock skew between
// agent replicas. The moment of issue is NotBefore plus this (see certIssuedAt).
const certBackdate = time.Minute

// certIssuedAt is when a client certificate was issued, as the enrollment epoch sees it.
func certIssuedAt(c *x509.Certificate) time.Time { return c.NotBefore.Add(certBackdate) }

// issueCertificate signs a client certificate for the tenant with the agent's client CA.
func (h *Handler) issueCertificate(tenant string, pub any) (*protobuf.CertificateResponse, error) {
	if h.caCertFile == "" || h.caKeyFile == "" {
		return nil, status.Error(codes.FailedPrecondition, "the agent has no client CA to sign with")
	}
	ca, key, caPEM, err := loadCA(h.caCertFile, h.caKeyFile)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "client CA: %v", err)
	}
	ttl := h.certTTL
	if ttl <= 0 {
		ttl = DefaultClientCertTTL
	}
	now := h.now()
	notAfter := now.Add(ttl)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		// Whatever subject the request asked for is ignored: the CN is the tenant.
		Subject:     pkix.Name{CommonName: tenant},
		NotBefore:   now.Add(-certBackdate),
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign: %v", err)
	}
	return &protobuf.CertificateResponse{
		CertificatePem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		ChainPem:       string(caPEM),
		NotAfterUnix:   notAfter.Unix(),
	}, nil
}

func loadCA(certFile, keyFile string) (*x509.Certificate, crypto.Signer, []byte, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, nil, fmt.Errorf("no certificate in %s", certFile)
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, nil, fmt.Errorf("no key in %s", keyFile)
	}
	var key any
	switch kb.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, nil, fmt.Errorf("the CA key cannot sign")
	}
	return ca, signer, certPEM, nil
}

// parseCSR reads a PKCS#10 request: valid signature (proof of possession), EC P-256 or stronger
// or RSA 2048 or more.
func parseCSR(pemStr string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil || (block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST") {
		return nil, fmt.Errorf("not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the request signature does not verify: %w", err)
	}
	switch k := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return nil, fmt.Errorf("EC key weaker than P-256")
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < minRSABits {
			return nil, fmt.Errorf("RSA key shorter than %d bits", minRSABits)
		}
	default:
		return nil, fmt.Errorf("the key must be EC (P-256 or stronger) or RSA (2048 bits or more)")
	}
	return csr, nil
}

// parseAccessKey reads a PKIX Ed25519 public key and returns its canonical PEM.
func parseAccessKey(pemStr string) ([]byte, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	if _, ok := pub.(ed25519.PublicKey); !ok {
		return nil, fmt.Errorf("the access key must be Ed25519")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: block.Bytes}), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sortedKeyIDs is for tests and listings.
func sortedKeyIDs(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
