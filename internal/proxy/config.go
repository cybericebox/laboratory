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
	Listen     string `env:"LISTEN_HTTPS"  envDefault:":443"`
	CookieName string `env:"SESSION_COOKIE_NAME" envDefault:"challenge"`
	// SessionSecret signs the proxy's own session cookie (HMAC-SHA256, at least 32
	// bytes). It is shared by all replicas and never leaves the cluster; the
	// platform does not know it and it is never the lab access key.
	SessionSecret string `env:"SESSION_SECRET,required"`
	// Instance names this replica in its traffic reports (the pod name).
	Instance       string        `env:"POD_NAME"`
	ReportInterval time.Duration `env:"REPORT_INTERVAL" envDefault:"1m"`
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
	return cfg, nil
}

func LoadWGConfig() (*WGConfig, error) {
	cfg := &WGConfig{}
	return cfg, config.Load(cfg)
}
