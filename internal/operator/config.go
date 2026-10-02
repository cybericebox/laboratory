package operator

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	// PublicVPNEndpoint is the host:port advertised to WireGuard clients (demux public address).
	// Required, no default: the domain comes only from configuration.
	PublicVPNEndpoint string `env:"PUBLIC_VPN_ENDPOINT,notEmpty"`
	// VPNServicePort is the UDP port the VPN server listens on. Matches VPN_SERVICE_PORT in the proxy.
	VPNServicePort int `env:"VPN_SERVICE_PORT" envDefault:"51820"`
	// BaseDomain is the cluster ingress base domain used for device exposure URLs.
	// Required, no default.
	BaseDomain string `env:"BASE_DOMAIN,notEmpty"`
	// ProxySourceCIDRs is an optional list of CIDR blocks whose traffic the proxy sends when
	// using hostNetwork (e.g. in Kind, the proxy runs as a hostNetwork pod and its source IP is
	// the node IP, not a pod IP, so namespace/label selectors in NetworkPolicy don't apply).
	// Each CIDR is added as an ipBlock peer in the generated web-exposure NetworkPolicy.
	// Format: comma-separated CIDR strings, e.g. "172.18.0.4/32".
	ProxySourceCIDRs []string `env:"PROXY_SOURCE_CIDRS" envSeparator:","`
	// VPNBaseNetwork is the base address space for per-lab VPN subnets, VPN half of 10.128.0.0/9 (e.g. "10.128.0.0/10").
	// Each lab is assigned a /24 child subnet by adding its index to the base address.
	VPNBaseNetwork string `env:"VPN_BASE_NETWORK" envDefault:"10.128.0.0/10"`
	// InetBaseNetwork is the base address space for per-lab internet/gateway subnets, internet half (e.g. "10.192.0.0/10").
	InetBaseNetwork string `env:"INET_BASE_NETWORK" envDefault:"10.192.0.0/10"`
	// VPNImage is the container image for per-LabGroup VPN pods. Required, no default: the chart passes
	// repository:tag (the tag is the chart appVersion unless set).
	VPNImage string `env:"VPN_IMAGE,notEmpty"`
	// GatewayImage is the container image for per-LabGroup gateway pods. Required, no default.
	GatewayImage string `env:"GATEWAY_IMAGE,notEmpty"`
	// GroupPods are the resources of the VPN and gateway pods of a new LabGroup (requests = limits).
	GroupPods grouppods.Config
	// NetConfigImage is the image for the optional device init-container that
	// assigns static IP/routes. Needs iproute2 + sh; node-agent image has both.
	// Required, no default.
	NetConfigImage string `env:"NETCONFIG_IMAGE,notEmpty"`
	// LabNodeSelectorJSON is a JSON-encoded map[string]string of nodeSelector labels
	// applied to all runtime lab pods (VPN, gateway, device).
	LabNodeSelectorJSON string `env:"LAB_NODE_SELECTOR" envDefault:"{}"`
	// LabTolerationsJSON is a JSON-encoded []corev1.Toleration applied to all runtime lab pods.
	LabTolerationsJSON string `env:"LAB_TOLERATIONS" envDefault:"[]"`
	// AgentEnabled gates creation of the management-agent RoleBinding in each
	// LabGroup namespace.
	AgentEnabled bool `env:"AGENT_ENABLED" envDefault:"false"`
	// AgentServiceAccount is the name of the management-agent ServiceAccount
	// bound by the per-group RoleBinding.
	AgentServiceAccount string `env:"AGENT_SERVICE_ACCOUNT" envDefault:"laboratory-agent"`
	// AgentServiceNamespace is the namespace of the management-agent ServiceAccount.
	AgentServiceNamespace string `env:"AGENT_SERVICE_NAMESPACE" envDefault:"laboratory-agent"`
	// OperatorServiceAccount and OperatorNamespace are the operator's own identity: in every LabGroup namespace it
	// binds that ServiceAccount to the ClusterRole of its working permissions (it has no cluster-wide write access).
	OperatorServiceAccount string `env:"OPERATOR_SERVICE_ACCOUNT" envDefault:"laboratory-controller-manager"`
	OperatorNamespace      string `env:"OPERATOR_NAMESPACE" envDefault:"laboratory-system"`
	// NetworkPolicyEnabled gates creation of the default-deny NetworkPolicy
	// baseline in each LabGroup namespace.
	NetworkPolicyEnabled bool `env:"NETWORK_POLICY_ENABLED" envDefault:"true"`
	// VPNStatsInterval is how often the VPN pod of a NEW group samples traffic statistics
	// (env STATS_INTERVAL of the pod).
	VPNStatsInterval time.Duration `env:"VPN_STATS_INTERVAL" envDefault:"30s"`
	// ImagePullSecrets lists registry Secrets (kubernetes.io/dockerconfigjson) in the
	// operator namespace, created outside the chart. The operator copies them into
	// every group namespace and sets them on the VPN, gateway and device pods, so a
	// private registry works for lab workloads. Empty means public images only.
	ImagePullSecrets []string `env:"IMAGE_PULL_SECRETS" envSeparator:","`
	// SupportEmail is the contact address shown to participants on the VPN
	// probe page. Passed to every per-LabGroup VPN pod as SUPPORT_EMAIL.
	// Required and not empty: the deploy always passes it.
	SupportEmail string `env:"SUPPORT_EMAIL,notEmpty"`

	// TenantEnrollmentTTL is how long an unused tenant enrollment token works.
	TenantEnrollmentTTL time.Duration `env:"TENANT_ENROLLMENT_TTL" envDefault:"24h"`

	// Scheduler: pods start through a conveyor so that a burst of labs does not
	// overload the cluster. See DEPLOY.md, "Scheduler".

	// SchedulerEnabled turns the queue on. Off: every pod starts as soon as its
	// object is created.
	SchedulerEnabled bool `env:"SCHEDULER_ENABLED" envDefault:"true"`
	// SchedulerMaxPods is the largest number of pods starting at once. A pod holds
	// its slot from dispatch until it is Ready or declared failed. 0 sets no limit
	// (prepull and the resource check still apply).
	SchedulerMaxPods int `env:"SCHEDULER_MAX_PODS" envDefault:"20"`
	// SchedulerStartupTimeout is how long a dispatched pod may take to become Ready
	// before it is declared failed and its slot is freed.
	SchedulerStartupTimeout time.Duration `env:"SCHEDULER_STARTUP_TIMEOUT" envDefault:"5m"`
	// SchedulerRestartThreshold is the number of restarts after which a pod that is
	// not Ready is declared failed before the timeout.
	SchedulerRestartThreshold int `env:"SCHEDULER_RESTART_THRESHOLD" envDefault:"5"`
	// SchedulerPlatformReservePercent is the platform reserve: the share of the schedulable CPU and memory
	// (allocatable, so after the kubelet's own reserve, and counted after DaemonSet and proxy requests) that user
	// labs can never consume. SchedulerPlatformReserveCPU and SchedulerPlatformReserveMemory are an extra
	// absolute reserve on EVERY schedulable node (quantities, "0" = none).
	SchedulerPlatformReservePercent int    `env:"SCHEDULER_PLATFORM_RESERVE_PERCENT" envDefault:"10"`
	SchedulerPlatformReserveCPU     string `env:"SCHEDULER_PLATFORM_RESERVE_CPU" envDefault:"0"`
	SchedulerPlatformReserveMemory  string `env:"SCHEDULER_PLATFORM_RESERVE_MEMORY" envDefault:"0"`
	// SchedulerResourceCheck holds a pod back while no node has room for its requests.
	SchedulerResourceCheck bool `env:"SCHEDULER_RESOURCE_CHECK" envDefault:"true"`
	// SchedulerPrepull pulls the images of a group onto the nodes before its first pod starts.
	SchedulerPrepull bool `env:"SCHEDULER_PREPULL" envDefault:"true"`
	// SchedulerPrepullTimeout bounds the wait for the image prepull; dispatch goes on after it.
	SchedulerPrepullTimeout time.Duration `env:"SCHEDULER_PREPULL_TIMEOUT" envDefault:"5m"`

	// DeviceDefaultCPU and DeviceDefaultMemory are the requests and limits of a
	// device container that declares neither (requests always equal limits, so
	// the pod is Guaranteed). Empty leaves such a device without resources.
	DeviceDefaultCPU    string `env:"DEVICE_DEFAULT_CPU" envDefault:"100m"`
	DeviceDefaultMemory string `env:"DEVICE_DEFAULT_MEMORY" envDefault:"256Mi"`

	// DeviceUserNamespaces runs device pods with hostUsers: false (default on; a hidden setting, not shown in any UI).
	// DeviceEphemeralStorage limits the writable layer, logs and emptyDirs of a device ("" or "0" = no limit).
	DeviceUserNamespaces   bool   `env:"DEVICE_USER_NAMESPACES" envDefault:"true"`
	DeviceEphemeralStorage string `env:"DEVICE_EPHEMERAL_STORAGE" envDefault:"2Gi"`

	// State is the device state persistence configuration (snapshot registry
	// and snapshot policy). Every field is optional; Enabled=false is today's
	// behaviour.
	State StateConfig

	// Cache is the image cache: lab images are pulled through the platform's
	// pull-through cache (zot with on-demand sync) instead of straight from the
	// upstream registries. Independent of State.
	Cache CacheConfig
}

// CacheConfig configures the image cache rewrite of lab image references.
type CacheConfig struct {
	Enabled bool `env:"IMAGE_CACHE_ENABLED" envDefault:"false"`
	// Prefix is host:port of the cache as the nodes see it (the node-agent's
	// localhost forwarder).
	Prefix string `env:"IMAGE_CACHE_PREFIX" envDefault:"localhost:5035"`
	// Registries are the upstream registries the cache serves; references to any
	// other registry are pulled directly.
	// PinTTL is how long a resolved image digest is remembered, so labs created in
	// the same wave run the same image even when the upstream tag moves.
	PinTTL     time.Duration `env:"IMAGE_CACHE_PIN_TTL" envDefault:"30m"`
	Registries []string      `env:"IMAGE_CACHE_REGISTRIES" envSeparator:"," envDefault:"docker.io,ghcr.io,quay.io,registry.k8s.io"`
}

// Rewriter returns the image reference rewriter; the zero one (no rewrite)
// when the cache is off.
func (c CacheConfig) Rewriter() imagecache.Rewriter {
	if !c.Enabled {
		return imagecache.Rewriter{}
	}
	return imagecache.Rewriter{Prefix: c.Prefix, Registries: c.Registries}
}

// StateConfig configures device state persistence: devices of new labs run as
// bare Pods whose writable layer the node-agent snapshots into the platform
// snapshot registry.
type StateConfig struct {
	Enabled bool `env:"STATE_PERSISTENCE_ENABLED" envDefault:"false"`
	// RegistryAddr is host:port of the snapshot registry Service, used by the
	// operator to drop snapshots (device reset, retention).
	RegistryAddr     string `env:"STATE_REGISTRY_ADDR"`
	RegistryUser     string `env:"STATE_REGISTRY_USER"`
	RegistryPassword string `env:"STATE_REGISTRY_PASSWORD"`
	// Debounce is how long a device's writable layer must stay quiet before a snapshot.
	Debounce time.Duration `env:"STATE_DEBOUNCE" envDefault:"5s"`
	// ExcludePaths are never snapshotted (comma-separated absolute paths).
	ExcludePaths []string `env:"STATE_EXCLUDE_PATHS" envSeparator:"," envDefault:"/tmp,/var/tmp,/run"`
	// WriteQuota is the quota per device, a Kubernetes quantity ("512Mi").
	WriteQuota string `env:"STATE_WRITE_QUOTA" envDefault:"512Mi"`
	// MaxFileSize: a regular file larger than this is left out of a snapshot (a Kubernetes quantity).
	MaxFileSize string `env:"STATE_MAX_FILE_SIZE" envDefault:"256Mi"`
	// MaxLayers is the snapshot layer count after which the chain is squashed.
	MaxLayers int32 `env:"STATE_MAX_LAYERS" envDefault:"10"`
	// Retention is how long the snapshots of a deleted lab are kept.
	Retention time.Duration `env:"STATE_RETENTION" envDefault:"168h"`
	// RetentionInterval is how often the retention sweep runs.
	RetentionInterval time.Duration `env:"STATE_RETENTION_INTERVAL" envDefault:"10m"`
}

// WriteQuotaBytes parses WriteQuota.
func (c StateConfig) WriteQuotaBytes() (int64, error) {
	q, err := resource.ParseQuantity(c.WriteQuota)
	if err != nil {
		return 0, fmt.Errorf("STATE_WRITE_QUOTA %q: %w", c.WriteQuota, err)
	}
	return q.Value(), nil
}

// MaxFileBytes parses MaxFileSize.
func (c StateConfig) MaxFileBytes() (int64, error) {
	q, err := resource.ParseQuantity(c.MaxFileSize)
	if err != nil {
		return 0, fmt.Errorf("STATE_MAX_FILE_SIZE %q: %w", c.MaxFileSize, err)
	}
	return q.Value(), nil
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	if err := config.Load(cfg); err != nil {
		return nil, err
	}
	if address, err := mail.ParseAddress(cfg.SupportEmail); err != nil || address.Address != cfg.SupportEmail {
		return nil, fmt.Errorf("SUPPORT_EMAIL %q is not a plain email address", cfg.SupportEmail)
	}
	if cfg.SchedulerMaxPods < 0 {
		return nil, fmt.Errorf("SCHEDULER_MAX_PODS must not be negative")
	}
	if cfg.SchedulerPlatformReservePercent < 0 || cfg.SchedulerPlatformReservePercent >= 100 {
		return nil, fmt.Errorf("SCHEDULER_PLATFORM_RESERVE_PERCENT must be in [0,100)")
	}
	for name, v := range map[string]string{"SCHEDULER_PLATFORM_RESERVE_CPU": cfg.SchedulerPlatformReserveCPU, "SCHEDULER_PLATFORM_RESERVE_MEMORY": cfg.SchedulerPlatformReserveMemory} {
		if q, err := resource.ParseQuantity(v); err != nil || q.Sign() < 0 {
			return nil, fmt.Errorf("%s %q is not a non-negative quantity", name, v)
		}
	}
	if cfg.SchedulerStartupTimeout <= 0 {
		return nil, fmt.Errorf("SCHEDULER_STARTUP_TIMEOUT must be positive")
	}
	if cfg.SchedulerRestartThreshold < 1 {
		return nil, fmt.Errorf("SCHEDULER_RESTART_THRESHOLD must be at least 1")
	}
	for name, v := range map[string]string{"DEVICE_DEFAULT_CPU": cfg.DeviceDefaultCPU, "DEVICE_DEFAULT_MEMORY": cfg.DeviceDefaultMemory} {
		if v == "" {
			continue
		}
		if _, err := resource.ParseQuantity(v); err != nil {
			return nil, fmt.Errorf("%s %q: %w", name, v, err)
		}
	}
	if v := cfg.DeviceEphemeralStorage; v != "" && v != "0" {
		if q, err := resource.ParseQuantity(v); err != nil || q.Sign() < 0 {
			return nil, fmt.Errorf("DEVICE_EPHEMERAL_STORAGE %q is not a quantity", v)
		}
	} else {
		cfg.DeviceEphemeralStorage = ""
	}
	if err := cfg.GroupPods.Validate(); err != nil {
		return nil, err
	}
	if _, err := cfg.State.WriteQuotaBytes(); err != nil {
		return nil, err
	}
	if _, err := cfg.State.MaxFileBytes(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseLabScheduling deserialises the JSON-encoded nodeSelector and tolerations
// from the operator config into typed values ready for pod specs.
func ParseLabScheduling(cfg *Config) (nodeSelector map[string]string, tolerations []corev1.Toleration, err error) {
	if err = json.Unmarshal([]byte(cfg.LabNodeSelectorJSON), &nodeSelector); err != nil {
		return nil, nil, fmt.Errorf("parse LAB_NODE_SELECTOR: %w", err)
	}
	if err = json.Unmarshal([]byte(cfg.LabTolerationsJSON), &tolerations); err != nil {
		return nil, nil, fmt.Errorf("parse LAB_TOLERATIONS: %w", err)
	}
	return nodeSelector, tolerations, nil
}
