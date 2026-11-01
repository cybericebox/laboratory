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

	// The egress of the labs (docs/specs/lab-internet-egress.md): a lab's TCP 80 and 443 end in the web proxy of the gateway, its port 53 in the
	// DNS answerer, and ICMP echo is the only thing forwarded.
	EgressHTTPPort  int `env:"EGRESS_HTTP_PORT" envDefault:"3180"`
	EgressTLSPort   int `env:"EGRESS_TLS_PORT" envDefault:"3443"`
	EgressDNSPort   int `env:"EGRESS_DNS_PORT" envDefault:"5353"`
	EgressStatsPort int `env:"EGRESS_STATS_PORT" envDefault:"9188"`
	// EgressDNSUpstreams are the resolvers the gateway asks (comma separated): public ones, since the gateway's own policy reaches the
	// public internet only and the lab must never learn names of the cluster.
	EgressDNSUpstreams string `env:"EGRESS_DNS_UPSTREAMS" envDefault:"1.1.1.1,9.9.9.9"`
	// EgressDNSQueriesPerSecond is the most DNS questions a lab may ask in a second.
	EgressDNSQueriesPerSecond float64 `env:"EGRESS_DNS_QUERIES_PER_SECOND" envDefault:"50"`
	// EgressHostNewPerSecond is the most new connections per second to one host name through this gateway.
	EgressHostNewPerSecond float64 `env:"EGRESS_HOST_NEW_PER_SECOND" envDefault:"5"`
	// EgressMaxConnSeconds is the longest a proxied connection lives.
	EgressMaxConnSeconds int `env:"EGRESS_MAX_CONN_SECONDS" envDefault:"600"`
	// EgressTunnelSuffixes replaces the built-in list of tunnel services (comma separated; "-" = none).
	EgressTunnelSuffixes string `env:"EGRESS_TUNNEL_SUFFIXES"`
	// EgressICMPPerSecond is the most echo requests per second a lab may send out.
	EgressICMPPerSecond int `env:"EGRESS_ICMP_PER_SECOND" envDefault:"5"`
	// DownloadKbps and UploadKbps are the per-lab ceilings of the internet path when the pod carries no bandwidth annotation (the bandwidth
	// protection of the operator is off): what the lab receives, what it sends.
	DownloadKbps int64 `env:"GATEWAY_DOWNLOAD_KBPS" envDefault:"51200"`
	UploadKbps   int64 `env:"GATEWAY_UPLOAD_KBPS" envDefault:"2048"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
