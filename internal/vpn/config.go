//go:build linux

package vpn

import (
	"net"
	"time"

	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	PrivateKey    string        `env:"PRIVATE_KEY,required"`
	ListenPort    int           `env:"LISTEN_PORT"    envDefault:"51820"`
	Namespace     string        `env:"NAMESPACE,required"`
	ClientSubnet  *net.IPNet    `env:"CLIENT_SUBNET,required"`
	VPNSupernet   *net.IPNet    `env:"VPN_SUPERNET,required"`
	StatsInterval time.Duration `env:"STATS_INTERVAL" envDefault:"30s"`
	WGInterface   string        `env:"WG_INTERFACE"   envDefault:"wg0"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
