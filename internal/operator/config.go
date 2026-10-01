package operator

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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
	// VPNImage is the container image for per-LabGroup VPN pods.
	VPNImage string `env:"VPN_IMAGE" envDefault:"cybericebox/laboratory-lab:latest"`
	// GatewayImage is the container image for per-LabGroup gateway pods.
	GatewayImage string `env:"GATEWAY_IMAGE" envDefault:"cybericebox/laboratory-lab:latest"`
	// NetConfigImage is the image for the optional device init-container that
	// assigns static IP/routes. Needs iproute2 + sh; node-agent image has both.
	NetConfigImage string `env:"NETCONFIG_IMAGE" envDefault:"cybericebox/laboratory-node-agent:latest"`
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
	// NetworkPolicyEnabled gates creation of the default-deny NetworkPolicy
	// baseline in each LabGroup namespace.
	NetworkPolicyEnabled bool `env:"NETWORK_POLICY_ENABLED" envDefault:"true"`
	// ImagePullSecrets lists registry Secrets (kubernetes.io/dockerconfigjson) in the
	// operator namespace, created outside the chart. The operator copies them into
	// every group namespace and sets them on the VPN, gateway and device pods, so a
	// private registry works for lab workloads. Empty means public images only.
	ImagePullSecrets []string `env:"IMAGE_PULL_SECRETS" envSeparator:","`
	// SupportEmail is the contact address shown to participants on the VPN
	// probe page. Passed to every per-LabGroup VPN pod as SUPPORT_EMAIL.
	SupportEmail string `env:"SUPPORT_EMAIL,required"`

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
	// SchedulerHeadroomPercent is the share of the schedulable CPU and memory that
	// must stay free after a pod is dispatched.
	SchedulerHeadroomPercent int `env:"SCHEDULER_HEADROOM_PERCENT" envDefault:"10"`
	// SchedulerResourceCheck holds a pod back while no node has room for its requests.
	SchedulerResourceCheck bool `env:"SCHEDULER_RESOURCE_CHECK" envDefault:"true"`
	// SchedulerPrepull pulls the images of a group onto the nodes before its first pod starts.
	SchedulerPrepull bool `env:"SCHEDULER_PREPULL" envDefault:"true"`
	// SchedulerPrepullTimeout bounds the wait for the image prepull; dispatch goes on after it.
	SchedulerPrepullTimeout time.Duration `env:"SCHEDULER_PREPULL_TIMEOUT" envDefault:"5m"`

	// DeviceDefaultCPU and DeviceDefaultMemory are the requests and limits of a
	// device container that declares neither (requests always equal limits, so
	// the pod is Guaranteed). Empty leaves such a device without resources.
	DeviceDefaultCPU    string `env:"DEVICE_DEFAULT_CPU" envDefault:"250m"`
	DeviceDefaultMemory string `env:"DEVICE_DEFAULT_MEMORY" envDefault:"256Mi"`

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
	if cfg.SchedulerHeadroomPercent < 0 || cfg.SchedulerHeadroomPercent >= 100 {
		return nil, fmt.Errorf("SCHEDULER_HEADROOM_PERCENT must be in [0,100)")
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
	if _, err := cfg.State.WriteQuotaBytes(); err != nil {
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
