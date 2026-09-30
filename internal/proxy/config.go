package proxy

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/cybericebox/laboratory/pkg/config"
)

type L7Config struct {
	TLSCertPath string `env:"TLS_CERT_PATH,required"`
	TLSKeyPath  string `env:"TLS_KEY_PATH,required"`
	// LabAccessPublicKeyPath is the Ed25519 public key (PKIX PEM) that verifies the
	// platform's lab access tokens (the handoff links).
	LabAccessPublicKeyPath string `env:"LAB_ACCESS_PUBLIC_KEY_PATH,required"`
	BaseDomain             string `env:"BASE_DOMAIN,required"`
	Listen                 string `env:"LISTEN_HTTPS"  envDefault:":443"`
	CookieName             string `env:"SESSION_COOKIE_NAME" envDefault:"challenge"`
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

// LoadLabAccessPublicKey reads the Ed25519 public key PEM from the configured
// path.
func (l *L7Config) LoadLabAccessPublicKey() (ed25519.PublicKey, error) {
	return ReadLabAccessPublicKey(l.LabAccessPublicKeyPath)
}

// ReadLabAccessPublicKey parses a PKIX PEM Ed25519 public key file.
func ReadLabAccessPublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read lab access public key %s: %w", path, err)
	}
	pub, err := ParseLabAccessPublicKey(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pub, nil
}

// ParseLabAccessPublicKey parses a PKIX PEM Ed25519 public key. The operator
// uses it to refuse a key the proxy could not load.
func ParseLabAccessPublicKey(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in the lab access public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse lab access public key: %w", err)
	}
	edPub, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("the lab access public key is not Ed25519")
	}
	return edPub, nil
}

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
