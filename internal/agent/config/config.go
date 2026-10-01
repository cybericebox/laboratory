package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

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
	// Monitoring bounds the shared journal behind the Monitoring stream.
	Monitoring MonitoringConfig
	// StatePersistence is the platform switch for device state persistence: a topology
	// with devices[].persistence.enabled is refused when it is off.
	StatePersistence bool `env:"AGENT_STATE_PERSISTENCE_ENABLED" envDefault:"false"`
	// Cache lets the agent prewarm the platform image cache.
	Cache CacheConfig
}

// CacheConfig configures PrewarmImages. Without Enabled the RPC fails with FailedPrecondition.
type CacheConfig struct {
	Enabled bool `env:"AGENT_CACHE_ENABLED" envDefault:"false"`
	// RegistryAddr is host:port of the cache (the registry Service) as the agent reaches it.
	RegistryAddr string `env:"AGENT_CACHE_REGISTRY_ADDR"`
	// Registries are the upstream registries the cache serves.
	Registries []string `env:"AGENT_CACHE_REGISTRIES" envSeparator:","`
	// PullSecrets are dockerconfigjson Secrets of PullSecretNamespace used to ask the
	// upstream registries for digests.
	PullSecrets         []string `env:"AGENT_PULL_SECRETS" envSeparator:","`
	PullSecretNamespace string   `env:"AGENT_PULL_SECRET_NAMESPACE" envDefault:"laboratory-system"`
	// LabNodeSelector is the JSON nodeSelector of lab pods: the nodes whose
	// architectures decide which platform of an image is warmed.
	LabNodeSelector string `env:"AGENT_LAB_NODE_SELECTOR" envDefault:"{}"`
	// PinTTL is how long a resolved digest is remembered (the tag is looked up again after it).
	PinTTL      time.Duration `env:"AGENT_CACHE_PIN_TTL" envDefault:"5m"`
	Concurrency int           `env:"AGENT_PREWARM_CONCURRENCY" envDefault:"4"`
	Timeout     time.Duration `env:"AGENT_PREWARM_TIMEOUT" envDefault:"10m"`
}

// MonitoringConfig sizes the Monitoring stream's shared poller and journal. A
// subscriber that lags more than SubscriberBuffer updates is dropped (it reconnects
// and resumes); a resume older than JournalSize updates or JournalAge gets a snapshot.
type MonitoringConfig struct {
	JournalSize      int           `env:"AGENT_MONITORING_JOURNAL_SIZE" envDefault:"10000"`
	JournalAge       time.Duration `env:"AGENT_MONITORING_JOURNAL_AGE" envDefault:"15m"`
	PollInterval     time.Duration `env:"AGENT_MONITORING_POLL_INTERVAL" envDefault:"1s"`
	SubscriberBuffer int           `env:"AGENT_MONITORING_SUBSCRIBER_BUFFER" envDefault:"256"`
}

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, err
	}
	return c, nil
}
