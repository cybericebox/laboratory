package grpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/profiles"
	"github.com/cybericebox/laboratory/internal/tenant"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Features are the platform's choices the backend needs to know, set once from the chart.
type Features struct {
	Lifecycle protobuf.LifecycleFeature
	// StatePersistence is the cluster switch; Debounce, ExcludePaths, WriteQuota and MaxFileSize are its cluster values
	// (bytes), the ceilings of a tenant's own limits.
	StatePersistence bool
	Debounce         time.Duration
	ExcludePaths     []string
	WriteQuota       int64
	MaxFileSize      int64
	TenantQuota      int64
	MaxEntries       int
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
	ProxySessionIdleTTL    time.Duration
	ProxySessionMaxTTL     time.Duration
	// DeviceProfiles are the enabled catalog profile IDs (internal/profiles).
	DeviceProfiles []string
	// Limits are the caps CreateLabs enforces.
	Limits limits.Limits
	// GroupPods is how a group's own pods are sized (the maximum CreateLabGroups accepts) and the default size without one.
	GroupPods GroupPodsFeature
}

// GroupPodsFeature is grouppods.Sizings plus the chart's default sizes.
type GroupPodsFeature struct {
	Sizing                     grouppods.Sizings
	DefaultVPN, DefaultGateway grouppods.Overhead
}

type featuresCache struct {
	mu        sync.Mutex
	m         map[string]featuresEntry
	refreshes tenantRefreshGate
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
		release, err := h.featCache.refreshes.acquire(ctx, name)
		if err != nil {
			return nil, err
		}
		defer release()
		h.featCache.mu.Lock()
		e, ok = h.featCache.m[name]
		h.featCache.mu.Unlock()
		if ok && time.Since(e.at) < capacityTTL {
			base = e.feat
		} else {
			ten, err := h.tenantObject(ctx, name)
			if err != nil {
				return nil, err
			}
			f := h.features
			quota, err := h.tenantQuota(ctx, ten)
			if err != nil {
				return nil, err
			}
			p := tenant.EffectivePersistence(ten, f.StatePersistence, f.WriteQuota, f.MaxFileSize, f.TenantQuota)
			base = &protobuf.FeaturesResponse{
				Tenant:    name,
				Lifecycle: proto.Clone(&f.Lifecycle).(*protobuf.LifecycleFeature),
				StatePersistence: &protobuf.StatePersistenceFeature{
					Available:          p.Allowed,
					DefaultDebounceMs:  f.Debounce.Milliseconds(),
					WriteQuotaBytes:    p.WriteQuota,
					MaxFileSizeBytes:   p.MaxFileSize,
					RegistryQuotaBytes: p.RegistryQuota,
					MaxEntries:         int32(f.MaxEntries),
					ExcludedPaths:      append([]string(nil), f.ExcludePaths...),
				},
				ImageCache:     &protobuf.ImageCacheFeature{Enabled: f.CacheEnabled, Registries: append([]string(nil), f.CacheRegistries...)},
				Scheduler:      &protobuf.SchedulerFeature{Enabled: f.SchedulerEnabled, MaxPods: f.SchedulerMaxPods},
				Endpoints:      &protobuf.EndpointsFeature{LabsDomain: f.LabsDomain, VpnEndpoint: f.VPNEndpoint},
				DeviceProfiles: append([]string(nil), f.DeviceProfiles...),
				Limits: &protobuf.LimitsFeature{
					Device: &protobuf.DeviceLimits{
						MaxCpuMillicores: f.Limits.DeviceMaxCPU, MaxMemoryBytes: f.Limits.DeviceMaxMemory,
						DefaultCpuMillicores: f.Limits.DeviceDefaultCPU, DefaultMemoryBytes: f.Limits.DeviceDefaultMemory,
					},
					Lab:    &protobuf.LabLimits{MaxDevices: int32(f.Limits.LabMaxDevices)},
					Group:  &protobuf.GroupLimits{MaxLabs: int32(f.Limits.GroupMaxLabs), MaxCpuMillicores: f.Limits.GroupMaxCPU, MaxMemoryBytes: f.Limits.GroupMaxMemory},
					Tenant: &protobuf.TenantLimits{MaxLabs: int32(f.Limits.TenantMaxLabs)},
				},
				GroupPods: &protobuf.GroupPodsFeature{
					SizingV2:       &protobuf.GroupPodsSizingV2{Profiles: []*protobuf.GroupPodsSizingProfile{grouppods.TestedPoint()}},
					Vpn:            podSizingProto(f.GroupPods.Sizing.VPN),
					Gateway:        podSizingProto(f.GroupPods.Sizing.Gateway),
					DefaultVpn:     &protobuf.PodSize{CpuMillicores: f.GroupPods.DefaultVPN.CPU, MemoryBytes: f.GroupPods.DefaultVPN.Memory},
					DefaultGateway: &protobuf.PodSize{CpuMillicores: f.GroupPods.DefaultGateway.CPU, MemoryBytes: f.GroupPods.DefaultGateway.Memory},
				},
				Constants: &protobuf.DeviceConstants{
					MaxInterfacesPerContainer: names.MaxContainerInterfaces, MaxPortsPerSwitchOrHub: names.MaxSwitchPorts, HardMaxDevicesPerLab: names.MaxLabDevices,
				},
				TenantQuota: &protobuf.TenantQuotaFeature{
					HasCpuQuota: quota.HasCPU, CpuQuotaMillicores: quota.CPU, HasMemoryQuota: quota.HasMemory, MemoryQuotaBytes: quota.Memory,
				},
				Proxy: &protobuf.ProxyFeature{
					AccessTokenMaxTtlSeconds: int64(f.ProxyAccessTokenMaxTTL.Seconds()), SessionMaxTtlSeconds: int64(f.ProxySessionMaxTTL.Seconds()), SessionIdleTtlSeconds: int64(f.ProxySessionIdleTTL.Seconds()),
				},
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			h.featCache.mu.Lock()
			if h.featCache.m == nil {
				h.featCache.m = map[string]featuresEntry{}
			}
			h.featCache.m[name] = featuresEntry{at: time.Now(), feat: base}
			h.featCache.mu.Unlock()
		}
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

// checkProfiles refuses a device whose security profile is not in the catalog or not enabled on this cluster. The
// old names (basic, service, net, debug) are aliases of the catalog IDs. Without a configured list nothing is checked.
func (h *Handler) checkProfiles(spec *laboratoryv1alpha1.LabSpec) error {
	enabled := h.features.DeviceProfiles
	if len(enabled) == 0 {
		return nil
	}
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue
		}
		name := string(d.SecurityPreset)
		if id, ok := profiles.Resolve(name); !ok {
			return fmt.Errorf("device %q: unknown security profile %q", d.Name, name)
		} else if !profiles.IsEnabled(name, enabled) {
			return fmt.Errorf("device %q: the security profile %q is not enabled on this cluster (enabled: %s)", d.Name, id, strings.Join(enabled, ", "))
		}
	}
	return nil
}

func podSizingProto(p grouppods.PodSizing) *protobuf.PodSizing {
	return &protobuf.PodSizing{
		BaseCpuMillicores: p.BaseCPU, BaseMemoryBytes: p.BaseMemory,
		PerUnitCpuMillicores: p.PerUnitCPU, PerUnitMemoryBytes: p.PerUnitMemory, MaxUnits: int32(p.MaxUnits),
	}
}
