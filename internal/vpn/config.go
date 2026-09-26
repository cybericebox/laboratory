//go:build linux

package vpn

import (
	"net"
	"time"

	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	PrivateKey     string        `env:"PRIVATE_KEY,required"`
	ListenPort     int           `env:"LISTEN_PORT"    envDefault:"51820"`
	Namespace      string        `env:"NAMESPACE,required"`
	ClientSubnet   *net.IPNet    `env:"CLIENT_SUBNET,required"`
	StatsInterval  time.Duration `env:"STATS_INTERVAL" envDefault:"30s"`
	WGInterface    string        `env:"WG_INTERFACE"     envDefault:"wg0"`
	VPNBaseNetwork string        `env:"VPN_BASE_NETWORK,required"`
	SupportEmail   string        `env:"SUPPORT_EMAIL"    envDefault:"support@cybericebox.com"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
