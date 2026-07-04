package config

import "github.com/caarlos0/env/v11"

type TLSConfig struct {
	Enabled  bool   `env:"AGENT_TLS_ENABLED" envDefault:"true"`
	CertFile string `env:"AGENT_TLS_CERT"`
	KeyFile  string `env:"AGENT_TLS_KEY"`
	CAFile   string `env:"AGENT_TLS_CA"`
}

type Config struct {
	GRPCPort         string   `env:"AGENT_GRPC_PORT" envDefault:"5454"`
	TLS              TLSConfig
	MTLSEnabled      bool     `env:"AGENT_MTLS_ENABLED" envDefault:"true"`
	AllowedClientCNs []string `env:"AGENT_ALLOWED_CLIENT_CNS" envSeparator:","`
}

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, err
	}
	return c, nil
}
