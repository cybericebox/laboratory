//go:build linux

package gateway

import (
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	Namespace         string `env:"NAMESPACE,required"`
	ExternalInterface string `env:"EXTERNAL_INTERFACE"  envDefault:"eth0"`
	InetBaseNetwork   string `env:"INET_BASE_NETWORK,required"`
	// EgressDenyCIDRs are the destinations the labs behind the gateway cannot reach (the internet gateway forwards
	// everything else); EgressAllowCIDRs are exceptions inside them. The operator passes the chart's lists.
	EgressDenyCIDRs  []string `env:"GATEWAY_EGRESS_DENY_CIDRS" envSeparator:"," envDefault:"169.254.0.0/16,127.0.0.0/8,0.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,192.0.0.0/24,198.18.0.0/15,224.0.0.0/3"`
	EgressAllowCIDRs []string `env:"GATEWAY_EGRESS_ALLOW_CIDRS" envSeparator:","`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
