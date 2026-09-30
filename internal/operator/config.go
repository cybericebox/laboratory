package operator

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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

	// Launch pacing: a new Lab is admitted from a queue so that a burst of labs
	// does not overload the cluster.

	// LaunchEnabled turns the queue on. Off: every lab is provisioned at once,
	// as before launch pacing existed.
	LaunchEnabled bool `env:"LAUNCH_ENABLED" envDefault:"true"`
	// LaunchMaxInFlight is the largest number of labs provisioning at once. A lab
	// counts from admission until it is Ready or LaunchWaveTimeout expires.
	// 0 sets no limit (prepull and the resource check still apply).
	LaunchMaxInFlight int `env:"LAUNCH_MAX_IN_FLIGHT" envDefault:"20"`
	// LaunchWaveTimeout is how long an admitted lab holds its slot if it is not Ready.
	LaunchWaveTimeout time.Duration `env:"LAUNCH_WAVE_TIMEOUT" envDefault:"3m"`
	// LaunchHeadroomPercent is the share of the schedulable CPU and memory that
	// must stay free after a lab is admitted.
	LaunchHeadroomPercent int `env:"LAUNCH_HEADROOM_PERCENT" envDefault:"10"`
	// LaunchResourceCheck gates admission on free cluster CPU and memory.
	LaunchResourceCheck bool `env:"LAUNCH_RESOURCE_CHECK" envDefault:"true"`
	// LaunchPrepull pulls the images of a lab class onto the nodes before its first wave.
	LaunchPrepull bool `env:"LAUNCH_PREPULL" envDefault:"true"`
	// LaunchPrepullTimeout bounds the wait for the image prepull; admission goes on after it.
	LaunchPrepullTimeout time.Duration `env:"LAUNCH_PREPULL_TIMEOUT" envDefault:"5m"`

	// DeviceDefaultCPU and DeviceDefaultMemory are the requests and limits of a
	// device container that declares neither (requests always equal limits, so
	// the pod is Guaranteed). Empty leaves such a device without resources.
	DeviceDefaultCPU    string `env:"DEVICE_DEFAULT_CPU" envDefault:"250m"`
	DeviceDefaultMemory string `env:"DEVICE_DEFAULT_MEMORY" envDefault:"256Mi"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	if err := config.Load(cfg); err != nil {
		return nil, err
	}
	if address, err := mail.ParseAddress(cfg.SupportEmail); err != nil || address.Address != cfg.SupportEmail {
		return nil, fmt.Errorf("SUPPORT_EMAIL %q is not a plain email address", cfg.SupportEmail)
	}
	if cfg.LaunchMaxInFlight < 0 {
		return nil, fmt.Errorf("LAUNCH_MAX_IN_FLIGHT must not be negative")
	}
	if cfg.LaunchHeadroomPercent < 0 || cfg.LaunchHeadroomPercent >= 100 {
		return nil, fmt.Errorf("LAUNCH_HEADROOM_PERCENT must be in [0,100)")
	}
	for name, v := range map[string]string{"DEVICE_DEFAULT_CPU": cfg.DeviceDefaultCPU, "DEVICE_DEFAULT_MEMORY": cfg.DeviceDefaultMemory} {
		if v == "" {
			continue
		}
		if _, err := resource.ParseQuantity(v); err != nil {
			return nil, fmt.Errorf("%s %q: %w", name, v, err)
		}
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
