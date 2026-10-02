package grpc

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/tenant"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Features are the platform's choices the backend needs to know, set once from the chart.
type Features struct {
	// StatePersistence is the cluster switch; Debounce, ExcludePaths, WriteQuota and MaxFileSize are its cluster values
	// (bytes), the ceilings of a tenant's own limits.
	StatePersistence bool
	Debounce         time.Duration
	ExcludePaths     []string
	WriteQuota       int64
	MaxFileSize      int64
	// CacheEnabled and CacheRegistries describe the image cache.
	CacheEnabled    bool
	CacheRegistries []string
	// SchedulerEnabled and SchedulerMaxPods describe the pod queue.
	SchedulerEnabled bool
	SchedulerMaxPods int32
	// LabsDomain and VPNEndpoint are the public hosts of lab endpoints and of WireGuard.
	LabsDomain  string
	VPNEndpoint string
	// ProxyAccessTokenMaxTTL and ProxySessionMaxTTL are the L7 proxy's limits.
	ProxyAccessTokenMaxTTL time.Duration
	ProxySessionMaxTTL     time.Duration
	// Limits are the caps CreateLabs enforces.
	Limits limits.Limits
}

type featuresCache struct {
	mu sync.Mutex
	m  map[string]featuresEntry
}

type featuresEntry struct {
	at   time.Time
	feat *protobuf.FeaturesResponse
}

// SetFeatures sets the platform features the agent reports (GetFeatures, Monitoring).
func (h *Handler) SetFeatures(f Features) {
	h.features = f
	h.statePersistence = f.StatePersistence
}

// GetFeatures is the caller's tenant view of what the laboratory offers.
func (h *Handler) GetFeatures(ctx context.Context, _ *protobuf.Empty) (*protobuf.FeaturesResponse, error) {
	return h.tenantFeatures(ctx)
}

func (h *Handler) tenantFeatures(ctx context.Context) (*protobuf.FeaturesResponse, error) {
	name := tenantOf(ctx)
	h.featCache.mu.Lock()
	e, ok := h.featCache.m[name]
	h.featCache.mu.Unlock()
	var base *protobuf.FeaturesResponse
	if ok && time.Since(e.at) < capacityTTL {
		base = e.feat
	} else {
		ten, err := h.tenantObject(ctx, name)
		if err != nil {
			return nil, err
		}
		f := h.features
		p := tenant.EffectivePersistence(ten, f.StatePersistence, f.WriteQuota, f.MaxFileSize)
		base = &protobuf.FeaturesResponse{
			Tenant: name,
			StatePersistence: &protobuf.StatePersistenceFeature{
				Available:         p.Allowed,
				DefaultDebounceMs: f.Debounce.Milliseconds(),
				WriteQuotaBytes:   p.WriteQuota,
				MaxFileSizeBytes:  p.MaxFileSize,
				ExcludedPaths:     append([]string(nil), f.ExcludePaths...),
			},
			ImageCache: &protobuf.ImageCacheFeature{Enabled: f.CacheEnabled, Registries: append([]string(nil), f.CacheRegistries...)},
			Scheduler:  &protobuf.SchedulerFeature{Enabled: f.SchedulerEnabled, MaxPods: f.SchedulerMaxPods},
			Endpoints:  &protobuf.EndpointsFeature{LabsDomain: f.LabsDomain, VpnEndpoint: f.VPNEndpoint},
			Limits: &protobuf.LimitsFeature{
				DeviceMaxCpuMillicores: f.Limits.DeviceMaxCPU, DeviceMaxMemoryBytes: f.Limits.DeviceMaxMemory,
				DeviceDefaultCpuMillicores: f.Limits.DeviceDefaultCPU, DeviceDefaultMemoryBytes: f.Limits.DeviceDefaultMemory,
				LabMaxDevices: int32(f.Limits.LabMaxDevices), LabMaxCpuMillicores: f.Limits.LabMaxCPU, LabMaxMemoryBytes: f.Limits.LabMaxMemory,
				TenantMaxLabs: int32(f.Limits.TenantMaxLabs),
			},
			Proxy: &protobuf.ProxyFeature{
				AccessTokenMaxTtlSeconds: int64(f.ProxyAccessTokenMaxTTL.Seconds()), SessionMaxTtlSeconds: int64(f.ProxySessionMaxTTL.Seconds()),
			},
		}
		h.featCache.mu.Lock()
		if h.featCache.m == nil {
			h.featCache.m = map[string]featuresEntry{}
		}
		h.featCache.m[name] = featuresEntry{at: time.Now(), feat: base}
		h.featCache.mu.Unlock()
	}
	// The certificate belongs to the connection, so it is added to a copy.
	out := proto.Clone(base).(*protobuf.FeaturesResponse)
	ttl := h.certTTL
	if ttl <= 0 {
		ttl = DefaultClientCertTTL
	}
	out.Certificate = &protobuf.CertificateFeature{NotAfterUnix: callerCertNotAfter(ctx), IssuedTtlSeconds: int64(ttl.Seconds())}
	return out, nil
}

// callerCertNotAfter is the expiry of the client certificate the call came with; 0 without one.
func callerCertNotAfter(ctx context.Context) int64 {
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			return info.State.PeerCertificates[0].NotAfter.Unix()
		}
	}
	return 0
}
