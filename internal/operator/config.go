package operator

import "github.com/cybericebox/laboratory/pkg/config"

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
	// VPNBaseNetwork is the base address space for per-lab VPN subnets (e.g. "10.8.0.0/16").
	// Each lab is assigned a /24 child subnet by adding its index to the base address.
	VPNBaseNetwork string `env:"VPN_BASE_NETWORK" envDefault:"10.8.0.0/10"`
	// InetBaseNetwork is the base address space for per-lab internet/gateway subnets (e.g. "10.9.0.0/10").
	InetBaseNetwork string `env:"INET_BASE_NETWORK" envDefault:"10.9.0.0/10"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
