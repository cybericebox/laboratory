package operator

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	// PublicVPNEndpoint is the host:port advertised to WireGuard clients (demux public address).
	PublicVPNEndpoint string `env:"PUBLIC_VPN_ENDPOINT"`
	// VPNServicePort is the UDP port the VPN server listens on. Matches VPN_SERVICE_PORT in the proxy.
	VPNServicePort int `env:"VPN_SERVICE_PORT" envDefault:"51820"`
	// BaseDomain is the cluster ingress base domain used for device exposure URLs.
	BaseDomain string `env:"BASE_DOMAIN"`
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
	// DHCPDNS is the DNS server address advertised to WireGuard clients via DHCP option (optional).
	DHCPDNS string `env:"DHCP_DNS"`
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
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
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
