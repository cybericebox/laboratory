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
	// tenant-<name>-access-keys of the tenants namespace (see l7.SecretKeys); there is no shared key.
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
}

type WGConfig struct {
	ListenAddr     string `env:"UDP_LISTEN_ADDR"  envDefault:":51820"`
	VPNServicePort int    `env:"VPN_SERVICE_PORT" envDefault:"51820"`
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
	return cfg, config.Load(cfg)
}
