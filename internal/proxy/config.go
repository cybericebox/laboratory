package proxy

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/cybericebox/laboratory/pkg/config"
)

type L7Config struct {
	TLSCertPath     string `env:"TLS_CERT_PATH,required"`
	TLSKeyPath      string `env:"TLS_KEY_PATH,required"`
	JWTPublicKeyPEM string `env:"JWT_PUBLIC_KEY,required"`
	BaseDomain      string `env:"BASE_DOMAIN,required"`
	Listen          string `env:"LISTEN_HTTPS"  envDefault:":443"`
	CookieName      string `env:"COOKIE_NAME"   envDefault:"challenge"`
}

type WGConfig struct {
	ListenAddr        string `env:"UDP_LISTEN_ADDR"    envDefault:":51820"`
	ExternalInterface string `env:"EXTERNAL_INTERFACE" envDefault:"eth0"`
	VPNServicePort    int    `env:"VPN_SERVICE_PORT"   envDefault:"51820"`
}

type Config struct {
	L7 L7Config
	WG WGConfig
}

func (l *L7Config) ParsedJWTPublicKey() (*rsa.PublicKey, error) {
	v := strings.ReplaceAll(l.JWTPublicKeyPEM, `\n`, "\n")
	block, _ := pem.Decode([]byte(v))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from JWT_PUBLIC_KEY")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("JWT_PUBLIC_KEY is not an RSA public key")
	}
	return rsaPub, nil
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
