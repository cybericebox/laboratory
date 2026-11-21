package proxy

import (
	"fmt"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/proxy/demux"
	"github.com/cybericebox/laboratory/pkg/config"
)

type L7Config struct {
	// The certificate and key of the wildcard Secret proxy-tls, where the chart mounts it.
	TLSCertPath string `env:"TLS_CERT_PATH" envDefault:"/etc/proxy/tls/tls.crt"`
	TLSKeyPath  string `env:"TLS_KEY_PATH" envDefault:"/etc/proxy/tls/tls.key"`
	// The access grants are verified with the access public keys of the tenants, kept in the Secrets
	// tenant-<name>-access-keys of the access keys namespace (see l7.SecretKeys); the proxy has no key of its own and signs nothing.
	BaseDomain string `env:"BASE_DOMAIN,notEmpty"`
	Listen     string `env:"LISTEN_HTTPS"  envDefault:":8443"`
	// CookieName is the host-only cookie a web host keeps its grant in (chart proxy.l7.grantCookieName). The default carries the __Host- prefix:
	// a browser then accepts it only when it is Secure, has Path=/ and no Domain, so a sibling site under the parent domain cannot set or shadow
	// it. The name is used everywhere (the cookie, the strip before the device, the filter of the device's own Set-Cookie).
	CookieName string `env:"GRANT_COOKIE_NAME" envDefault:"__Host-cib_grant"`
	// GrantMaxTTL is the longest exp - iat of a grant the proxy accepts (chart proxy.l7.grantMaxTTL); the agent reports it to the backend.
	GrantMaxTTL time.Duration `env:"GRANT_MAX_TTL" envDefault:"8h"`
	// PlatformOrigins are the pages that may post a grant (https://host[:port] or https://*.base[:port], as the embedding lists): the form
	// comes from the platform page, so the Origin of the post must match one of them. Empty: every post is refused.
	PlatformOrigins []string `env:"PLATFORM_ORIGINS" envSeparator:","`
	// HealthAddr is where the readiness and liveness probes are served (the l7 container; the wg-demux container uses another port).
	HealthAddr string `env:"HEALTH_ADDR" envDefault:":8081"`
	// Instance names this replica in its traffic reports (the pod name).
	Instance       string        `env:"POD_NAME"`
	ReportInterval time.Duration `env:"REPORT_INTERVAL" envDefault:"1m"`
	// LiveMaxLifetime caps one request or upgraded (WebSocket) connection; LiveCheckInterval is how often the
	// open ones are checked against the access policy again, so a lock cuts them within about that time.
	LiveMaxLifetime   time.Duration `env:"LIVE_MAX_LIFETIME" envDefault:"12h"`
	LiveCheckInterval time.Duration `env:"LIVE_CHECK_INTERVAL" envDefault:"10s"`
	// SlowRequest is the upstream latency above which the access log flags a request as slow.
	SlowRequest time.Duration `env:"SLOW_REQUEST" envDefault:"2s"`
	// HTTP server limits (before any routing or authentication, so they stop slow-header and slow-body clients that
	// would otherwise hold a connection and a goroutine forever): ReadHeaderTimeout bounds the request headers, ReadTimeout
	// the whole request including the body, IdleTimeout a kept-alive connection with no request, MaxHeaderBytes the headers.
	// There is no write timeout: responses stream, and an upgraded (WebSocket) connection lives at most LiveMaxLifetime.
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"10s"`
	ReadTimeout       time.Duration `env:"READ_TIMEOUT" envDefault:"5m"`
	IdleTimeout       time.Duration `env:"IDLE_TIMEOUT" envDefault:"2m"`
	MaxHeaderBytes    int           `env:"MAX_HEADER_BYTES" envDefault:"65536"`
	// What a client can hold open (R-17): MaxConnections is the connections of the server in all; LivePerClient, LivePerGroup and
	// LiveTotal the requests in flight (upgraded connections included) per client of a group, per group and in all; AuthRate and
	// AuthBurst limit the handoff path per peer address (all peers together may do twenty times that). 0 = unlimited.
	// MaxConnections follows the memory limit of the chart (512Mi): a connection with a request in flight costs about 105 KiB, so
	// 2500 are about 260 MiB, and with the informer caches and headroom they stay under the 80% soft limit. LiveTotal is not above
	// it: a request in flight holds a connection. Above these numbers add replicas, do not raise the cap without the memory.
	MaxConnections int     `env:"MAX_CONNECTIONS" envDefault:"2500"`
	LivePerClient  int     `env:"LIVE_PER_CLIENT" envDefault:"200"`
	LivePerGroup   int     `env:"LIVE_PER_GROUP" envDefault:"1000"`
	LiveTotal      int     `env:"LIVE_TOTAL" envDefault:"2500"`
	AuthRate       float64 `env:"AUTH_RATE" envDefault:"5"`
	AuthBurst      int     `env:"AUTH_BURST" envDefault:"20"`

	// The console (docs/specs/2026-11-12-device-access-and-checks.md): TerminalEnabled serves it on a host of its own per device (names.AccessHostLabel, the kind of access in the last character of the first label) under
	// BaseDomain, relayed to the device access service pods of TerminalNamespace on TerminalPort. The limits mirror the chart values of the
	// service (terminal.limits.*) and are enforced at the proxy as well.
	TerminalEnabled   bool          `env:"TERMINAL_ENABLED" envDefault:"false"`
	TerminalNamespace string        `env:"TERMINAL_SERVICE_NAMESPACE"`
	TerminalPort      int           `env:"TERMINAL_SERVICE_PORT" envDefault:"8080"`
	TerminalPerClient int           `env:"TERMINAL_PER_CLIENT" envDefault:"3"`
	TerminalIdle      time.Duration `env:"TERMINAL_IDLE_TIMEOUT" envDefault:"30m"`
	TerminalLifetime  time.Duration `env:"TERMINAL_MAX_LIFETIME" envDefault:"8h"`
	// TerminalEmbedAllow and TerminalEmbedDeny are the lists of sites that may (and may not) show the console page in a frame; see
	// l7.EmbedMatcher. Empty allow: no embedding.
	TerminalEmbedAllow []string `env:"TERMINAL_EMBED_ALLOW" envSeparator:","`
	TerminalEmbedDeny  []string `env:"TERMINAL_EMBED_DENY" envSeparator:","`
}

type WGConfig struct {
	// ListenAddr is the UDP address the demux binds; empty = ":" + names.WireGuardPort. The VPN pods listen on names.WireGuardPort too.
	ListenAddr string `env:"UDP_LISTEN_ADDR"`
	// HealthAddr is where the readiness and liveness probes are served.
	HealthAddr string `env:"HEALTH_ADDR" envDefault:":8082"`
	// The demux reads a public UDP port shared by every team, so what a stranger can make it hold or spend is capped
	// (see demux.Limits). There is no limit per source address: one NAT hides a whole event behind one address.
	// MaxEntries: conntrack entries in total (two per session). PartialTTL: how long a handshake in progress keeps its entry.
	MaxEntries int           `env:"DEMUX_MAX_ENTRIES" envDefault:"100000"`
	PartialTTL time.Duration `env:"DEMUX_PARTIAL_TTL" envDefault:"15s"`
	// All sources together may start this many handshakes per second (each costs a scan of the groups' keys) and send this many
	// packets per second that match no session or come from an unexpected address (index guessing, spoofing, roaming).
	GlobalHandshakeRate  float64       `env:"DEMUX_GLOBAL_HANDSHAKE_RATE" envDefault:"2000"`
	GlobalHandshakeBurst int           `env:"DEMUX_GLOBAL_HANDSHAKE_BURST" envDefault:"4000"`
	MissRate             float64       `env:"DEMUX_MISS_RATE" envDefault:"2000"`
	MissBurst            int           `env:"DEMUX_MISS_BURST" envDefault:"4000"`
	RoamInterval         time.Duration `env:"DEMUX_ROAM_INTERVAL" envDefault:"5s"`
	// One session may send SessionRate packets per second, so that one participant cannot use up the shared demux.
	// Readers is how many goroutines read the socket.
	SessionRate  float64 `env:"DEMUX_SESSION_RATE" envDefault:"15000"`
	SessionBurst int     `env:"DEMUX_SESSION_BURST" envDefault:"30000"`
	Readers      int     `env:"DEMUX_READERS" envDefault:"4"`
	// Mode is how transport packets are moved: userspace, or in the kernel by a TC or XDP program (needs NodeName).
	Mode demux.Mode `env:"DEMUX_MODE" envDefault:"userspace"`
	// PinPath is where the BPF maps are pinned, so a restart keeps the sessions.
	PinPath  string `env:"DEMUX_BPF_PIN_PATH" envDefault:"/sys/fs/bpf/cybericebox/wg-demux"`
	NodeName string `env:"NODE_NAME"`
	HostIP   string `env:"HOST_IP"`
	// ServiceName and ServiceNamespace name the Service whose address the clients' handshakes arrive at.
	ServiceName      string        `env:"DEMUX_SERVICE_NAME" envDefault:"laboratory-proxy-wg"`
	ServiceNamespace string        `env:"DEMUX_SERVICE_NAMESPACE" envDefault:"laboratory-wg"`
	MetricsAddr      string        `env:"DEMUX_METRICS_ADDR" envDefault:":8083"`
	Resync           time.Duration `env:"DEMUX_BPF_RESYNC" envDefault:"10s"`
}

func LoadL7Config() (*L7Config, error) {
	cfg := &L7Config{}
	if err := config.Load(cfg); err != nil {
		return cfg, err
	}
	if cfg.MaxConnections < 0 || cfg.LivePerClient < 0 || cfg.LivePerGroup < 0 || cfg.LiveTotal < 0 || cfg.AuthRate < 0 || cfg.AuthBurst < 0 {
		return cfg, fmt.Errorf("MAX_CONNECTIONS, LIVE_PER_CLIENT, LIVE_PER_GROUP, LIVE_TOTAL, AUTH_RATE and AUTH_BURST must not be negative")
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.MaxHeaderBytes <= 0 {
		return cfg, fmt.Errorf("READ_HEADER_TIMEOUT, READ_TIMEOUT, IDLE_TIMEOUT and MAX_HEADER_BYTES must be positive")
	}
	if cfg.GrantMaxTTL <= 0 {
		return cfg, fmt.Errorf("GRANT_MAX_TTL must be positive")
	}
	if cfg.TerminalEnabled {
		if cfg.TerminalNamespace == "" || cfg.TerminalPort < 1 || cfg.TerminalPort > 65535 || cfg.TerminalPerClient < 0 {
			return cfg, fmt.Errorf("TERMINAL_SERVICE_NAMESPACE and TERMINAL_SERVICE_PORT are required with the console")
		}
	}
	return cfg, nil
}

func LoadWGConfig() (*WGConfig, error) {
	cfg := &WGConfig{}
	if err := config.Load(cfg); err != nil {
		return cfg, err
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", names.WireGuardPort)
	}
	switch cfg.Mode {
	case demux.ModeUserspace:
	case demux.ModeTC, demux.ModeXDP:
		if cfg.NodeName == "" {
			return cfg, fmt.Errorf("NODE_NAME is required in the %s mode", cfg.Mode)
		}
	default:
		return cfg, fmt.Errorf("DEMUX_MODE must be userspace, tc or xdp, not %q", cfg.Mode)
	}
	if cfg.Resync <= 0 {
		return cfg, fmt.Errorf("DEMUX_BPF_RESYNC must be positive")
	}
	if cfg.MaxEntries <= 0 || cfg.PartialTTL <= 0 || cfg.MissRate <= 0 || cfg.MissBurst <= 0 || cfg.RoamInterval <= 0 ||
		cfg.GlobalHandshakeRate <= 0 || cfg.GlobalHandshakeBurst <= 0 || cfg.SessionRate <= 0 || cfg.SessionBurst <= 0 || cfg.Readers <= 0 {
		return cfg, fmt.Errorf("the DEMUX_* limits must be positive")
	}
	return cfg, nil
}
