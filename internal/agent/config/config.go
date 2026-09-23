package config

import "github.com/caarlos0/env/v11"

// ServerTLSConfig is the agent's own TLS identity — a key pair only, no CA.
type ServerTLSConfig struct {
	Enabled  bool   `env:"AGENT_TLS_ENABLED" envDefault:"true"`
	CertFile string `env:"AGENT_TLS_CERT"`
	KeyFile  string `env:"AGENT_TLS_KEY"`
}

// MTLSConfig verifies CLIENT certificates against a separate client CA.
type MTLSConfig struct {
	Enabled          bool     `env:"AGENT_MTLS_ENABLED" envDefault:"true"`
	ClientCAFile     string   `env:"AGENT_MTLS_CLIENT_CA"`
	AllowedClientCNs []string `env:"AGENT_ALLOWED_CLIENT_CNS" envSeparator:","`
}

type Config struct {
	GRPCPort string `env:"AGENT_GRPC_PORT" envDefault:"5454"`
	// AgentID is a stable, deployment-scoped identity used to make monitoring
	// observations idempotent at the platform boundary.
	AgentID   string `env:"AGENT_ID" envDefault:"laboratory-agent"`
	ServerTLS ServerTLSConfig
	MTLS      MTLSConfig
}

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, err
	}
	return c, nil
}
