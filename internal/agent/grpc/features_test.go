package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

var testFeatures = Features{
	StatePersistence: true, Debounce: 5 * time.Second, ExcludePaths: []string{"/tmp", "/run"},
	WriteQuota: 512 << 20, MaxFileSize: 256 << 20,
	CacheEnabled: true, CacheRegistries: []string{"docker.io", "ghcr.io"},
	SchedulerEnabled: true, SchedulerMaxPods: 20,
	LabsDomain: "labs.example.com", VPNEndpoint: "vpn.example.com:51820",
	ProxyAccessTokenMaxTTL: 60 * time.Second, ProxySessionMaxTTL: 24 * time.Hour,
}

func featuresHandler(t *testing.T, f Features, tenants ...*laboratoryv1alpha1.Tenant) *Handler {
	t.Helper()
	h := tenantHandler(t, tenants)
	h.SetFeatures(f)
	return h
}

func TestFeaturesReportThePlatformChoices(t *testing.T) {
	h := featuresHandler(t, testFeatures, newTenantTenant("a", true, nil))
	got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	p := got.GetStatePersistence()
	if got.GetTenant() != "a" || !p.GetAvailable() || p.GetDefaultDebounceMs() != 5000 || p.GetWriteQuotaBytes() != 512<<20 ||
		p.GetMaxFileSizeBytes() != 256<<20 || len(p.GetExcludedPaths()) != 2 {
		t.Fatalf("persistence: %+v", got)
	}
	if c := got.GetImageCache(); !c.GetEnabled() || len(c.GetRegistries()) != 2 {
		t.Fatalf("cache: %+v", c)
	}
	if s := got.GetScheduler(); !s.GetEnabled() || s.GetMaxPods() != 20 {
		t.Fatalf("scheduler: %+v", s)
	}
	if e := got.GetEndpoints(); e.GetLabsDomain() != "labs.example.com" || e.GetVpnEndpoint() != "vpn.example.com:51820" {
		t.Fatalf("endpoints: %+v", e)
	}
	if px := got.GetProxy(); px.GetAccessTokenMaxTtlSeconds() != 60 || px.GetSessionMaxTtlSeconds() != 86400 {
		t.Fatalf("proxy: %+v", px)
	}
	if got.GetCertificate().GetIssuedTtlSeconds() != int64(DefaultClientCertTTL.Seconds()) {
		t.Fatalf("certificate: %+v", got.GetCertificate())
	}
}

// Persistence is available only when the cluster enables it AND the tenant is allowed; a tenant's own limits are
// capped by the cluster's.
func TestFeaturesPersistenceFollowsTenantAndCluster(t *testing.T) {
	small := newTenantTenant("small", true, nil)
	small.Spec.Persistence.WriteQuota, small.Spec.Persistence.MaxFileSize = "100Mi", "1Gi"
	h := featuresHandler(t, testFeatures, newTenantTenant("no", false, nil), small)
	no, _ := h.GetFeatures(asClient("no"), &protobuf.Empty{})
	if no.GetStatePersistence().GetAvailable() {
		t.Fatal("a tenant that is not allowed")
	}
	sm, _ := h.GetFeatures(asClient("small"), &protobuf.Empty{})
	if p := sm.GetStatePersistence(); !p.GetAvailable() || p.GetWriteQuotaBytes() != 100<<20 || p.GetMaxFileSizeBytes() != 256<<20 {
		t.Fatalf("limits: below the ceiling is kept, above it is capped: %+v", p)
	}
	off := testFeatures
	off.StatePersistence = false
	h2 := featuresHandler(t, off, small)
	if got, _ := h2.GetFeatures(asClient("small"), &protobuf.Empty{}); got.GetStatePersistence().GetAvailable() {
		t.Fatal("the cluster switch is off")
	}
}

func TestFeaturesCarryTheCallersCertificateExpiry(t *testing.T) {
	h := featuresHandler(t, testFeatures, newTenantTenant("a", true, nil))
	h.SetClientCA("", "", 48*time.Hour)
	notAfter := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "a"}, NotAfter: notAfter}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})
	got, err := h.GetFeatures(ctx, &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCertificate().GetNotAfterUnix() != notAfter.Unix() || got.GetCertificate().GetIssuedTtlSeconds() != 48*3600 {
		t.Fatalf("certificate: %+v", got.GetCertificate())
	}
	// The cached platform part is shared, the certificate is not: another connection of the same tenant has its own.
	other, _ := h.GetFeatures(asClient("a"), &protobuf.Empty{})
	if other.GetCertificate().GetNotAfterUnix() == notAfter.Unix() {
		t.Fatal("the certificate leaked through the cache")
	}
	if !proto.Equal(got.GetStatePersistence(), other.GetStatePersistence()) {
		t.Fatal("the platform part must be the same")
	}
}
