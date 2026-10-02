package config

import (
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/limits"
)

// ServerTLSConfig is the agent's own TLS identity — a key pair only, no CA.
type ServerTLSConfig struct {
	Enabled  bool   `env:"AGENT_TLS_ENABLED" envDefault:"true"`
	CertFile string `env:"AGENT_TLS_CERT"`
	KeyFile  string `env:"AGENT_TLS_KEY"`
}

// ServerLimits bound what a caller can cost the agent (see grpc.Server).
type ServerLimits struct {
	// MaxConcurrentStreams per connection, MaxConnectionAge (a connection is replaced after it, which also re-reads the client
	// certificate), and the keepalive ping rate the server tolerates.
	MaxConcurrentStreams  int           `env:"AGENT_MAX_CONCURRENT_STREAMS" envDefault:"64"`
	MaxConnectionAge      time.Duration `env:"AGENT_MAX_CONNECTION_AGE" envDefault:"1h"`
	MaxConnectionAgeGrace time.Duration `env:"AGENT_MAX_CONNECTION_AGE_GRACE" envDefault:"1m"`
	KeepaliveMinTime      time.Duration `env:"AGENT_KEEPALIVE_MIN_TIME" envDefault:"10s"`
	// StreamRecheck is how often a running stream (Monitoring, snapshot export) is authorized again.
	StreamRecheck time.Duration `env:"AGENT_STREAM_RECHECK" envDefault:"30s"`
	// The Enroll server, open to callers without a client certificate: the largest message, calls per second (all callers
	// together), and what all of them together may hold open. There is no per-address limit: behind the gateway every caller has the
	// same source address.
	EnrollMaxMessage  int           `env:"AGENT_ENROLL_MAX_MESSAGE_BYTES" envDefault:"65536"`
	EnrollRate        float64       `env:"AGENT_ENROLL_RATE" envDefault:"5"`
	EnrollBurst       int           `env:"AGENT_ENROLL_BURST" envDefault:"10"`
	HandshakeTimeout  time.Duration `env:"AGENT_HANDSHAKE_TIMEOUT" envDefault:"10s"`
	MaxHandshakes     int           `env:"AGENT_MAX_HANDSHAKES" envDefault:"64"`
	MaxAnonymousConns int           `env:"AGENT_MAX_ANONYMOUS_CONNS" envDefault:"256"`
	// ShutdownGrace is how long the agent lets running calls finish after SIGTERM (a CreateLabGroupClients that has made its key must be able
	// to answer with it) before it closes what is left, long-lived streams included. Keep terminationGracePeriodSeconds above it.
	ShutdownGrace time.Duration `env:"AGENT_SHUTDOWN_GRACE" envDefault:"30s"`
	// RenewMinInterval is how often one tenant may renew its client certificate.
	RenewMinInterval time.Duration `env:"AGENT_RENEW_MIN_INTERVAL" envDefault:"10s"`
}

// MTLSConfig verifies CLIENT certificates against a separate client CA.
type MTLSConfig struct {
	Enabled      bool   `env:"AGENT_MTLS_ENABLED" envDefault:"true"`
	ClientCAFile string `env:"AGENT_MTLS_CLIENT_CA"`
	// ClientCAKeyFile is the private key of that CA: the agent signs the client certificates of
	// enrolled tenants with it (Enroll, RenewCertificate).
	ClientCAKeyFile string `env:"AGENT_MTLS_CLIENT_CA_KEY"`
	// ClientCertTTL is how long an issued client certificate is valid.
	ClientCertTTL time.Duration `env:"AGENT_CLIENT_CERT_TTL" envDefault:"720h"`
}

type Config struct {
	GRPCPort string `env:"AGENT_GRPC_PORT" envDefault:"5454"`
	// AgentID is a stable, deployment-scoped identity used to make monitoring
	// observations idempotent at the platform boundary.
	AgentID   string `env:"AGENT_ID" envDefault:"laboratory-agent"`
	ServerTLS ServerTLSConfig
	MTLS      MTLSConfig
	// AllowInsecure lets the agent run with mTLS off, where every caller is the default tenant. It is for local development
	// only and has to be asked for by name (the chart sets it from agent.allowInsecure).
	AllowInsecure bool `env:"AGENT_ALLOW_INSECURE" envDefault:"false"`
	// Server holds the limits of the gRPC front.
	Server ServerLimits
	// Monitoring bounds the shared journal behind the Monitoring stream.
	Monitoring MonitoringConfig
	// LabNodeSelector and LabTolerations (JSON) describe the nodes lab pods run on: they decide
	// which platform of an image is warmed and what a percentage tenant quota is a percentage of.
	LabNodeSelector string `env:"AGENT_LAB_NODE_SELECTOR" envDefault:"{}"`
	LabTolerations  string `env:"AGENT_LAB_TOLERATIONS" envDefault:"[]"`
	// PlatformReservePercent, PlatformReserveCPU and PlatformReserveMemory are the operator's scheduler reserve (the same chart
	// values, scheduler.platformReserve*): the agent applies them to the per-node room it reports.
	PlatformReservePercent int    `env:"SCHEDULER_PLATFORM_RESERVE_PERCENT" envDefault:"10"`
	PlatformReserveCPU     string `env:"SCHEDULER_PLATFORM_RESERVE_CPU" envDefault:"0"`
	PlatformReserveMemory  string `env:"SCHEDULER_PLATFORM_RESERVE_MEMORY" envDefault:"0"`
	// PackingReservePercent is the hidden packing reserve (chart scheduler.packingReservePercent): the capacity reported to the
	// tenant is net of it. Tenants never see it.
	PackingReservePercent int `env:"SCHEDULER_PACKING_RESERVE_PERCENT" envDefault:"15"`
	// TenantStatusInterval is how often the status (reserved, used) of every Tenant is refreshed.
	TenantStatusInterval time.Duration `env:"AGENT_TENANT_STATUS_INTERVAL" envDefault:"30s"`
	// GroupPods are the resources of the VPN and gateway pods of a group (the same values as the operator's):
	// their sum is the service overhead GetCapacity reports.
	GroupPods grouppods.Config
	// GroupSizing is how the pods of a group are sized (the chart's vpn.sizing and inetGateway.sizing): reported in GetFeatures, and the
	// maximum of the sizes CreateLabGroups accepts.
	GroupSizing grouppods.Sizing
	// StatePersistence is the platform switch for device state persistence: a topology
	// with devices[].persistence.enabled is refused when it is off.
	StatePersistence bool `env:"AGENT_STATE_PERSISTENCE_ENABLED" envDefault:"false"`
	// State are the cluster values of state persistence (the chart's devices.statePersistence, the same as the
	// operator's): GetFeatures reports them, a tenant's own limits are capped by them.
	State StateConfig
	// Scheduler and Endpoints are reported by GetFeatures (the same chart values as the operator's).
	Scheduler SchedulerConfig
	// ProxyAccessTokenMaxTTL and ProxySessionMaxTTL are the L7 proxy's limits (chart proxy.l7.*), reported by GetFeatures.
	ProxyAccessTokenMaxTTL time.Duration `env:"AGENT_PROXY_ACCESS_TOKEN_MAX_TTL" envDefault:"60s"`
	ProxySessionIdleTTL    time.Duration `env:"AGENT_PROXY_SESSION_IDLE_TTL" envDefault:"24h"`
	ProxySessionMaxTTL     time.Duration `env:"AGENT_PROXY_SESSION_MAX_TTL" envDefault:"168h"`
	// DeviceProfiles are the catalog profile IDs this cluster offers (internal/profiles). The agent reports them in
	// GetFeatures and refuses a lab whose device asks for another.
	DeviceProfiles []string `env:"AGENT_DEVICE_PROFILES" envSeparator:"," envDefault:"standard,extended"`
	// ReleaseNamespace is where the operator, node-agents and proxy publish their error events (the chart's release namespace).
	ReleaseNamespace string `env:"AGENT_RELEASE_NAMESPACE" envDefault:"laboratory-system"`
	// Limits are the caps on devices, labs and tenants the agent enforces on CreateLabs and reports.
	Limits limits.Config
	// BaseDomain is the domain of the lab web endpoints; PublicVPNEndpoint is host:port of WireGuard.
	BaseDomain        string `env:"AGENT_BASE_DOMAIN"`
	PublicVPNEndpoint string `env:"AGENT_PUBLIC_VPN_ENDPOINT"`
	// RegistryAddr is host:port of the platform registry (zot) as the agent reaches it:
	// snapshot export reads the device snapshots from it. Empty: the export fails with FailedPrecondition.
	RegistryAddr string `env:"AGENT_REGISTRY_ADDR"`
	// RegistryUser and RegistryPassword are the reader account of the registry: the snapshots are not anonymous.
	RegistryUser     string `env:"AGENT_REGISTRY_USER"`
	RegistryPassword string `env:"AGENT_REGISTRY_PASSWORD"`
	// Cache lets the agent prewarm the platform image cache.
	Cache CacheConfig
	// ImageDeny are "registry/repository-prefix" entries no tenant may use (the platform's
	// private repositories); a tenant's own allow list narrows the rest.
	ImageDeny []string `env:"AGENT_IMAGE_DENY" envSeparator:","`
}

// StateConfig mirrors the state persistence values of the operator.
type StateConfig struct {
	Debounce     time.Duration `env:"AGENT_STATE_DEBOUNCE" envDefault:"5s"`
	ExcludePaths []string      `env:"AGENT_STATE_EXCLUDE_PATHS" envSeparator:"," envDefault:"/tmp,/var/tmp,/run"`
	// WriteQuota and MaxFileSize are Kubernetes quantities.
	WriteQuota  string `env:"AGENT_STATE_WRITE_QUOTA" envDefault:"512Mi"`
	MaxFileSize string `env:"AGENT_STATE_MAX_FILE_SIZE" envDefault:"256Mi"`
	// TenantQuota is what all of one tenant's snapshots may take in the registry together; MaxEntries caps one layer.
	TenantQuota string `env:"AGENT_STATE_TENANT_QUOTA" envDefault:"10Gi"`
	MaxEntries  int    `env:"AGENT_STATE_MAX_ENTRIES" envDefault:"100000"`
}

// SchedulerConfig mirrors the operator's scheduler switch and width.
type SchedulerConfig struct {
	Enabled bool `env:"AGENT_SCHEDULER_ENABLED" envDefault:"true"`
	MaxPods int  `env:"AGENT_SCHEDULER_MAX_PODS" envDefault:"20"`
}

// CacheConfig configures PrewarmImages. Without Enabled the RPC fails with FailedPrecondition.
type CacheConfig struct {
	Enabled bool `env:"AGENT_CACHE_ENABLED" envDefault:"false"`
	// RegistryAddr is host:port of the cache (the registry Service) as the agent reaches it.
	RegistryAddr string `env:"AGENT_CACHE_REGISTRY_ADDR"`
	// NodePrefix is host:port of the cache as the nodes see it (the node-agent's localhost
	// forwarder); a tenant image that names it is refused.
	NodePrefix string `env:"AGENT_CACHE_NODE_PREFIX"`
	// Registries are the upstream registries the cache serves.
	Registries []string `env:"AGENT_CACHE_REGISTRIES" envSeparator:","`
	// PullSecrets are dockerconfigjson Secrets of PullSecretNamespace used to ask the
	// upstream registries for digests.
	PullSecrets         []string `env:"AGENT_PULL_SECRETS" envSeparator:","`
	PullSecretNamespace string   `env:"AGENT_PULL_SECRET_NAMESPACE" envDefault:"laboratory-system"`
	// PinTTL is how long a resolved digest is remembered (the tag is looked up again after it).
	PinTTL      time.Duration `env:"AGENT_CACHE_PIN_TTL" envDefault:"30m"`
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
	// MaxStreamsPerTenant caps the Monitoring streams one tenant may hold open (0 = unlimited).
	MaxStreamsPerTenant int `env:"AGENT_MONITORING_MAX_STREAMS_PER_TENANT" envDefault:"8"`
}

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, err
	}
	return c, nil
}
