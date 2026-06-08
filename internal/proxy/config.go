package proxy

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/cybericebox/laboratory/pkg/config"
)

type L7Config struct {
	TLSCertPath        string `env:"TLS_CERT_PATH,required"`
	TLSKeyPath         string `env:"TLS_KEY_PATH,required"`
	JWTPublicKeyPath   string `env:"JWT_PUBLIC_KEY_PATH,required"`
	BaseDomain         string `env:"BASE_DOMAIN,required"`
	Listen             string `env:"LISTEN_HTTPS"  envDefault:":443"`
	CookieName         string `env:"COOKIE_NAME"   envDefault:"challenge"`
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

// LoadJWTPublicKey reads the RSA public key PEM from the configured path.
// Called on each request so that kubelet secret-volume updates are picked up
// without a pod restart.
func (l *L7Config) LoadJWTPublicKey() (*rsa.PublicKey, error) {
	data, err := os.ReadFile(l.JWTPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read JWT public key %s: %w", l.JWTPublicKeyPath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", l.JWTPublicKeyPath)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("JWT public key is not RSA")
	}
	return rsaPub, nil
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}

func LoadL7Config() (*L7Config, error) {
	cfg := &L7Config{}
	return cfg, config.Load(cfg)
}

func LoadWGConfig() (*WGConfig, error) {
	cfg := &WGConfig{}
	return cfg, config.Load(cfg)
}
