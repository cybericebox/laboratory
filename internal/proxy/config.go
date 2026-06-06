package proxy

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"strings"

	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	TLSCertPath   string       `env:"TLS_CERT_PATH,required"`
	TLSKeyPath    string       `env:"TLS_KEY_PATH,required"`
	JWTPublicKey  *rsa.PublicKey `env:"JWT_PUBLIC_KEY,required"`
	BaseDomain    string       `env:"BASE_DOMAIN,required"`
	ListenHTTPS   string       `env:"LISTEN_HTTPS"    envDefault:":443"`
	CookieName    string       `env:"COOKIE_NAME"     envDefault:"challenge"`
	UDPListenAddr string       `env:"UDP_LISTEN_ADDR" envDefault:":51820"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg, map[reflect.Type]config.ParserFunc{
		reflect.TypeOf((*rsa.PublicKey)(nil)): func(v string) (interface{}, error) {
			v = strings.ReplaceAll(v, `\n`, "\n")
			block, _ := pem.Decode([]byte(v))
			if block == nil {
				return nil, fmt.Errorf("failed to decode PEM block")
			}
			pub, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse public key: %w", err)
			}
			rsaPub, ok := pub.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("not an RSA public key")
			}
			return rsaPub, nil
		},
	})
}
