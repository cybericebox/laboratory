package proxy

import (
	"fmt"
	"time"

	"github.com/cybericebox/laboratory/pkg/config"
)

type L7Config struct {
	TLSCertPath string `env:"TLS_CERT_PATH,required"`
	TLSKeyPath  string `env:"TLS_KEY_PATH,required"`
	// The handoff links are verified with the access public keys of the tenants, kept in the Secrets
	// tenant-<name>-access-keys of the access keys namespace (see l7.SecretKeys); there is no shared key.
	BaseDomain string `env:"BASE_DOMAIN,notEmpty"`
	Listen     string `env:"LISTEN_HTTPS"  envDefault:":8443"`
	CookieName string `env:"SESSION_COOKIE_NAME" envDefault:"challenge"`
	// SessionSecret signs the proxy's own session cookie (HMAC-SHA256, at least 32
	// bytes). It is shared by all replicas and never leaves the cluster; the
	// platform does not know it and it is never the lab access key.
	SessionSecret string `env:"SESSION_SECRET,required"`
	// Instance names this replica in its traffic reports (the pod name).
	Instance       string        `env:"POD_NAME"`
	ReportInterval time.Duration `env:"REPORT_INTERVAL" envDefault:"1m"`
	// AccessTokenMaxTTL is the longest exp - iat of a handoff link the proxy accepts; SessionMaxTTL is the
	// longest its own session cookie lives, whatever the link asks for. The agent reports both to the backend.
	AccessTokenMaxTTL time.Duration `env:"ACCESS_TOKEN_MAX_TTL" envDefault:"60s"`
	// The session is sliding: it expires SessionIdleTTL after the last request, the cookie is re-issued only when
	// less than SessionRenewBefore of it remains, and it ends SessionMaxTTL after the handoff at the latest (and
	// never past the link's sess).
	SessionIdleTTL     time.Duration `env:"SESSION_IDLE_TTL" envDefault:"24h"`
	SessionRenewBefore time.Duration `env:"SESSION_RENEW_BEFORE" envDefault:"1h"`
	SessionMaxTTL      time.Duration `env:"SESSION_MAX_TTL" envDefault:"168h"`
	// LiveMaxLifetime caps one request or upgraded (WebSocket) connection; LiveCheckInterval is how often the
	// open ones are checked against the access policy again, so a lock cuts them within about that time.
	LiveMaxLifetime   time.Duration `env:"LIVE_MAX_LIFETIME" envDefault:"12h"`
	LiveCheckInterval time.Duration `env:"LIVE_CHECK_INTERVAL" envDefault:"10s"`
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
	MaxConnections int     `env:"MAX_CONNECTIONS" envDefault:"4000"`
	LivePerClient  int     `env:"LIVE_PER_CLIENT" envDefault:"200"`
	LivePerGroup   int     `env:"LIVE_PER_GROUP" envDefault:"1000"`
	LiveTotal      int     `env:"LIVE_TOTAL" envDefault:"8000"`
	AuthRate       float64 `env:"AUTH_RATE" envDefault:"5"`
	AuthBurst      int     `env:"AUTH_BURST" envDefault:"20"`
}

type WGConfig struct {
	ListenAddr     string `env:"UDP_LISTEN_ADDR"  envDefault:":51820"`
	VPNServicePort int    `env:"VPN_SERVICE_PORT" envDefault:"51820"`
	// The demux reads a public UDP port shared by every team, so what a stranger can make it hold or spend is capped
	// (see demux.Limits): the conntrack entries in total and per client address, handshake initiations and
	// unmatched packets per second per source address, and how often one session may change address.
	MaxEntries          int           `env:"DEMUX_MAX_ENTRIES" envDefault:"100000"`
	MaxEntriesPerSource int           `env:"DEMUX_MAX_ENTRIES_PER_SOURCE" envDefault:"128"`
	HandshakeRate       float64       `env:"DEMUX_HANDSHAKE_RATE" envDefault:"20"`
	HandshakeBurst      int           `env:"DEMUX_HANDSHAKE_BURST" envDefault:"50"`
	MissRate            float64       `env:"DEMUX_MISS_RATE" envDefault:"50"`
	MissBurst           int           `env:"DEMUX_MISS_BURST" envDefault:"100"`
	RoamInterval        time.Duration `env:"DEMUX_ROAM_INTERVAL" envDefault:"5s"`
	MaxSources          int           `env:"DEMUX_MAX_SOURCES" envDefault:"100000"`
	// All sources together may start this many handshakes per second (each costs a scan of the groups' keys, whoever pays for
	// it), one session may send SessionRate packets per second and one client address OwnerRate in all its sessions, so that one
	// participant cannot use up the shared demux. Readers is how many goroutines read the socket.
	GlobalHandshakeRate  float64 `env:"DEMUX_GLOBAL_HANDSHAKE_RATE" envDefault:"2000"`
	GlobalHandshakeBurst int     `env:"DEMUX_GLOBAL_HANDSHAKE_BURST" envDefault:"4000"`
	SessionRate          float64 `env:"DEMUX_SESSION_RATE" envDefault:"15000"`
	SessionBurst         int     `env:"DEMUX_SESSION_BURST" envDefault:"30000"`
	OwnerRate            float64 `env:"DEMUX_OWNER_RATE" envDefault:"40000"`
	OwnerBurst           int     `env:"DEMUX_OWNER_BURST" envDefault:"80000"`
	Readers              int     `env:"DEMUX_READERS" envDefault:"4"`
}

// MinSessionSecretLen is the shortest accepted SESSION_SECRET.
const MinSessionSecretLen = 32

func LoadL7Config() (*L7Config, error) {
	cfg := &L7Config{}
	if err := config.Load(cfg); err != nil {
		return cfg, err
	}
	if len(cfg.SessionSecret) < MinSessionSecretLen {
		return cfg, fmt.Errorf("SESSION_SECRET must be at least %d bytes", MinSessionSecretLen)
	}
	if cfg.MaxConnections < 0 || cfg.LivePerClient < 0 || cfg.LivePerGroup < 0 || cfg.LiveTotal < 0 || cfg.AuthRate < 0 || cfg.AuthBurst < 0 {
		return cfg, fmt.Errorf("MAX_CONNECTIONS, LIVE_PER_CLIENT, LIVE_PER_GROUP, LIVE_TOTAL, AUTH_RATE and AUTH_BURST must not be negative")
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.MaxHeaderBytes <= 0 {
		return cfg, fmt.Errorf("READ_HEADER_TIMEOUT, READ_TIMEOUT, IDLE_TIMEOUT and MAX_HEADER_BYTES must be positive")
	}
	if cfg.SessionIdleTTL <= 0 || cfg.SessionMaxTTL <= 0 || cfg.SessionRenewBefore < 0 || cfg.SessionRenewBefore >= cfg.SessionIdleTTL {
		return cfg, fmt.Errorf("SESSION_IDLE_TTL and SESSION_MAX_TTL must be positive and SESSION_RENEW_BEFORE must be shorter than SESSION_IDLE_TTL")
	}
	return cfg, nil
}

func LoadWGConfig() (*WGConfig, error) {
	cfg := &WGConfig{}
	if err := config.Load(cfg); err != nil {
		return cfg, err
	}
	if cfg.MaxEntries <= 0 || cfg.MaxEntriesPerSource <= 0 || cfg.HandshakeRate <= 0 || cfg.HandshakeBurst <= 0 || cfg.MissRate <= 0 || cfg.MissBurst <= 0 ||
		cfg.RoamInterval <= 0 || cfg.MaxSources <= 0 || cfg.GlobalHandshakeRate <= 0 || cfg.GlobalHandshakeBurst <= 0 ||
		cfg.SessionRate <= 0 || cfg.SessionBurst <= 0 || cfg.OwnerRate <= 0 || cfg.OwnerBurst <= 0 || cfg.Readers <= 0 {
		return cfg, fmt.Errorf("the DEMUX_* limits must be positive")
	}
	return cfg, nil
}
