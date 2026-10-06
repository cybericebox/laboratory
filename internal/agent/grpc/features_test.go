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
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

var testFeatures = Features{
	StatePersistence: true, Debounce: 5 * time.Second, ExcludePaths: []string{"/tmp", "/run"},
	WriteQuota: 512 << 20, MaxFileSize: 256 << 20, TenantQuota: 10 << 30, MaxEntries: 100000,
	CacheEnabled: true, CacheRegistries: []string{"docker.io", "ghcr.io"},
	SchedulerEnabled: true, SchedulerMaxPods: 20,
	LabsDomain: "labs.example.com", VPNEndpoint: "vpn.example.com:51820",
	ProxyAccessTokenMaxTTL: 60 * time.Second, ProxySessionMaxTTL: 24 * time.Hour,
	GroupPods: GroupPodsFeature{
		Sizing: grouppods.Sizings{
			VPN:     grouppods.PodSizing{BaseCPU: 20, BaseMemory: 64 << 20, PerUnitCPU: 6, PerUnitMemory: 20 << 20, MaxUnits: 20},
			Gateway: grouppods.PodSizing{BaseCPU: 5, BaseMemory: 16 << 20, PerUnitCPU: 2, PerUnitMemory: 4 << 20, MaxUnits: 50},
		},
		DefaultVPN: grouppods.Overhead{CPU: 100, Memory: 320 << 20}, DefaultGateway: grouppods.Overhead{CPU: 10, Memory: 32 << 20},
	},
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
		p.GetMaxFileSizeBytes() != 256<<20 || p.GetRegistryQuotaBytes() != 10<<30 || p.GetMaxEntries() != 100000 || len(p.GetExcludedPaths()) != 2 {
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

// The device model's constants, the sizing of a group's own pods and the tenant's quota are reported to the backend.
func TestFeaturesReportTheDeviceModelAndGroupPodSizing(t *testing.T) {
	ten := newTenantTenant("a", true, nil)
	ten.Spec.Quota = &laboratoryv1alpha1.TenantQuota{CPU: "4", Memory: "8Gi"}
	h := featuresHandler(t, testFeatures, ten)
	got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if c := got.GetConstants(); c.GetMaxInterfacesPerContainer() != 16 || c.GetMaxPortsPerSwitchOrHub() != 48 || c.GetHardMaxDevicesPerLab() != 64 {
		t.Fatalf("constants: %+v", c)
	}
	gp := got.GetGroupPods()
	if v := gp.GetVpn(); v.GetBaseCpuMillicores() != 20 || v.GetBaseMemoryBytes() != 64<<20 || v.GetPerUnitCpuMillicores() != 6 ||
		v.GetPerUnitMemoryBytes() != 20<<20 || v.GetMaxUnits() != 20 {
		t.Fatalf("vpn sizing: %+v", v)
	}
	if g := gp.GetGateway(); g.GetBaseCpuMillicores() != 5 || g.GetPerUnitMemoryBytes() != 4<<20 || g.GetMaxUnits() != 50 {
		t.Fatalf("gateway sizing: %+v", g)
	}
	if d := gp.GetDefaultVpn(); d.GetCpuMillicores() != 100 || d.GetMemoryBytes() != 320<<20 {
		t.Fatalf("default vpn: %+v", d)
	}
	if d := gp.GetDefaultGateway(); d.GetCpuMillicores() != 10 || d.GetMemoryBytes() != 32<<20 {
		t.Fatalf("default gateway: %+v", d)
	}
	if q := got.GetTenantQuota(); !q.GetHasCpuQuota() || q.GetCpuQuotaMillicores() != 4000 || !q.GetHasMemoryQuota() || q.GetMemoryQuotaBytes() != 8<<30 {
		t.Fatalf("tenant quota: %+v", q)
	}
	// a tenant without a quota has no limit
	free, _ := featuresHandler(t, testFeatures, newTenantTenant("b", true, nil)).GetFeatures(asClient("b"), &protobuf.Empty{})
	if q := free.GetTenantQuota(); q.GetHasCpuQuota() || q.GetHasMemoryQuota() {
		t.Fatalf("tenant quota without a limit: %+v", q)
	}
}
