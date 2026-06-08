//go:build linux

package gateway

import (
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	Namespace         string `env:"NAMESPACE,required"`
	ExternalInterface string `env:"EXTERNAL_INTERFACE"  envDefault:"eth0"`
	DHCPDNS           string `env:"DHCP_DNS"`
	InetBaseNetwork string `env:"INET_BASE_NETWORK,required"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
