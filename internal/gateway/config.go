//go:build linux

package gateway

import (
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	Namespace         string `env:"NAMESPACE,required"`
	ExternalInterface string `env:"EXTERNAL_INTERFACE"  envDefault:"eth0"`
	InetBaseNetwork   string `env:"INET_BASE_NETWORK,required"`
	// MaxConnectionsPerLab and NewConnectionsPerSecondPerLab limit what one lab may hold open and open per second towards the outside
	// (0 = no limit): GATEWAY_MAX_CONNECTIONS_PER_LAB, GATEWAY_NEW_CONNECTIONS_PER_SECOND_PER_LAB.
	MaxConnectionsPerLab          int `env:"GATEWAY_MAX_CONNECTIONS_PER_LAB"`
	NewConnectionsPerSecondPerLab int `env:"GATEWAY_NEW_CONNECTIONS_PER_SECOND_PER_LAB"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
