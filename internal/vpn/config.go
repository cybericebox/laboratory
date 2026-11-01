//go:build linux

package vpn

import (
	"net"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	PrivateKey    string        `env:"PRIVATE_KEY,required"`
	ListenPort    int           `env:"LISTEN_PORT"`
	Namespace     string        `env:"NAMESPACE,required"`
	ClientSubnet  *net.IPNet    `env:"CLIENT_SUBNET,required"`
	StatsInterval time.Duration `env:"STATS_INTERVAL" envDefault:"30s"`
	WGInterface   string        `env:"WG_INTERFACE"     envDefault:"wg0"`
	// Firewall is how the pod forwards between the participants and the labs: "iptables" (the relation chains, with connection tracking) or
	// "nft" (nftables sets, no connection tracking: see nft_forward_linux.go). VPN_FIREWALL.
	Firewall string `env:"VPN_FIREWALL" envDefault:"iptables"`
	// ExternalInterface is the uplink of the pod (the pod network), the one interface from which the WireGuard port is reachable:
	// the proxy delivers the clients' packets there. The lab interfaces (lab<N>) never are.
	ExternalInterface string `env:"EXTERNAL_INTERFACE" envDefault:"eth0"`
	VPNBaseNetwork    string `env:"VPN_BASE_NETWORK,required"`
	SupportEmail      string `env:"SUPPORT_EMAIL,required"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{ListenPort: names.WireGuardPort}
	return cfg, config.Load(cfg)
}
