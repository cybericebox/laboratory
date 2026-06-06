package proxy

import (
	"crypto/ed25519"

	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	TLSCertPath   string            `env:"TLS_CERT_PATH,required"`
	TLSKeyPath    string            `env:"TLS_KEY_PATH,required"`
	Ed25519PubKey ed25519.PublicKey `env:"ED25519_PUBLIC_KEY,required"`
	BaseDomain    string            `env:"BASE_DOMAIN,required"`
	ListenHTTPS   string            `env:"LISTEN_HTTPS"    envDefault:":443"`
	CookieName    string            `env:"COOKIE_NAME"     envDefault:"challenge"`
	UDPListenAddr string            `env:"UDP_LISTEN_ADDR" envDefault:":51820"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
