package config

import (
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
)

// ServerTLSConfig is the agent's own TLS identity — a key pair only, no CA. TLS is always on; the files are where the chart mounts the
// certificate of the agent (cert-manager Secret laboratory-agent-server-tls).
type ServerTLSConfig struct {
	CertFile string `env:"AGENT_TLS_CERT" envDefault:"/tls/tls.crt"`
	KeyFile  string `env:"AGENT_TLS_KEY" envDefault:"/tls/tls.key"`
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

// MTLSConfig verifies CLIENT certificates against a separate client CA (mTLS is on unless Config.AllowInsecure). The files are where the
// chart mounts the client CA (cert-manager Secret laboratory-agent-ca).
type MTLSConfig struct {
	ClientCAFile string `env:"AGENT_MTLS_CLIENT_CA" envDefault:"/ca/tls.crt"`
	// ClientCAKeyFile is the private key of that CA: the agent signs the client certificates of
	// enrolled tenants with it (Enroll, RenewCertificate).
	ClientCAKeyFile string `env:"AGENT_MTLS_CLIENT_CA_KEY" envDefault:"/ca/tls.key"`
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
	// AllowInsecure is the one switch that turns the client-certificate check off: every caller is then the default tenant. It is for
	// local development only and has to be asked for by name (the chart sets it from agent.allowInsecure). TLS itself stays on.
	AllowInsecure bool `env:"AGENT_ALLOW_INSECURE" envDefault:"false"`
	// Server holds the limits of the gRPC front.
	Server ServerLimits
	// LabNodeSelector and LabTolerations (JSON) describe the nodes lab pods run on: they decide
	// which platform of an image is warmed and what a percentage tenant quota is a percentage of.
	// The names are the operator's (LAB_NODE_SELECTOR, LAB_TOLERATIONS): one chart value, one name.
	LabNodeSelector string `env:"LAB_NODE_SELECTOR" envDefault:"{}"`
	LabTolerations  string `env:"LAB_TOLERATIONS" envDefault:"[]"`
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
	StatePersistence bool `env:"STATE_PERSISTENCE_ENABLED" envDefault:"false"`
	// State are the cluster values of state persistence (the chart's devices.statePersistence, the same as the
	// operator's): GetFeatures reports them, a tenant's own limits are capped by them.
	State StateConfig
	// Scheduler and Endpoints are reported by GetFeatures (the same chart values as the operator's).
	Scheduler SchedulerConfig
	// ProxyAccessTokenMaxTTL and ProxySessionMaxTTL are the L7 proxy's limits (chart proxy.l7.*), reported by GetFeatures. They are
	// read under the proxy's own names (the same variables of the same chart values), not as a second copy.
	ProxyAccessTokenMaxTTL time.Duration `env:"ACCESS_TOKEN_MAX_TTL" envDefault:"60s"`
	ProxySessionIdleTTL    time.Duration `env:"SESSION_IDLE_TTL" envDefault:"24h"`
	ProxySessionMaxTTL     time.Duration `env:"SESSION_MAX_TTL" envDefault:"168h"`
	// DeviceProfiles are the catalog profile IDs this cluster offers (internal/profiles). The agent reports them in
	// GetFeatures and refuses a lab whose device asks for another.
	DeviceProfiles []string `env:"AGENT_DEVICE_PROFILES" envSeparator:"," envDefault:"standard,extended"`
	// Limits are the caps on devices, labs and tenants the agent enforces on CreateLabs and reports.
	Limits limits.Config
	// BaseDomain is the domain of the lab web endpoints; PublicVPNEndpoint is host:port of WireGuard. The operator's names and the
	// operator's values (operator.baseDomain, operator.publicVPNEndpoint): the chart passes one value to both.
	BaseDomain        string `env:"BASE_DOMAIN"`
	PublicVPNEndpoint string `env:"PUBLIC_VPN_ENDPOINT"`
	// RegistryAddr is host:port of the platform registry (zot) as the agent reaches it: snapshot export reads the device snapshots from
	// it, and PrewarmImages makes the cache fetch through it. Empty = names.RegistryServiceAddr (the chart always installs it there).
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

// StateConfig is the state persistence values of the operator, under the operator's names (STATE_*): one chart value, one name.
type StateConfig struct {
	Debounce     time.Duration `env:"STATE_DEBOUNCE" envDefault:"5s"`
	ExcludePaths []string      `env:"STATE_EXCLUDE_PATHS" envSeparator:"," envDefault:"/tmp,/var/tmp,/run"`
	// WriteQuota and MaxFileSize are Kubernetes quantities.
	WriteQuota  string `env:"STATE_WRITE_QUOTA" envDefault:"512Mi"`
	MaxFileSize string `env:"STATE_MAX_FILE_SIZE" envDefault:"256Mi"`
	// TenantQuota is what all of one tenant's snapshots may take in the registry together; MaxEntries caps one layer.
	TenantQuota string `env:"STATE_TENANT_QUOTA" envDefault:"10Gi"`
	MaxEntries  int    `env:"STATE_MAX_ENTRIES" envDefault:"100000"`
}

// SchedulerConfig is the operator's scheduler switch and width, under the operator's names (SCHEDULER_*).
type SchedulerConfig struct {
	Enabled bool `env:"SCHEDULER_ENABLED" envDefault:"true"`
	MaxPods int  `env:"SCHEDULER_MAX_PODS" envDefault:"20"`
}

// CacheConfig configures PrewarmImages. Without Enabled the RPC fails with FailedPrecondition. The names are the operator's
// (IMAGE_CACHE_*, IMAGE_PULL_SECRETS): one chart value, one name.
type CacheConfig struct {
	Enabled bool `env:"IMAGE_CACHE_ENABLED" envDefault:"false"`
	// NodePrefix is host:port of the cache as the nodes see it (the node-agent's localhost forwarder); a tenant image that names it
	// is refused. Empty = names.RegistryNodePrefix.
	NodePrefix string `env:"IMAGE_CACHE_PREFIX"`
	// Registries are the upstream registries the cache serves.
	Registries []string `env:"IMAGE_CACHE_REGISTRIES" envSeparator:"," envDefault:"docker.io,ghcr.io,quay.io,registry.k8s.io"`
	// PullSecrets are dockerconfigjson Secrets of the release namespace used to ask the upstream registries for digests.
	PullSecrets []string `env:"IMAGE_PULL_SECRETS" envSeparator:","`
	// PinTTL is how long a resolved digest is remembered (the tag is looked up again after it).
	PinTTL      time.Duration `env:"IMAGE_CACHE_PIN_TTL" envDefault:"30m"`
	Concurrency int           `env:"AGENT_PREWARM_CONCURRENCY" envDefault:"4"`
	Timeout     time.Duration `env:"AGENT_PREWARM_TIMEOUT" envDefault:"10m"`
}

// MonitoringConfig sizes the Monitoring stream's shared poller and journal. A
// subscriber that lags more than SubscriberBuffer updates is dropped (it reconnects
// and resumes); a resume older than JournalSize updates or JournalAge gets a snapshot.
type MonitoringConfig struct {
	JournalSize      int
	JournalAge       time.Duration
	PollInterval     time.Duration
	SubscriberBuffer int
	// MaxStreamsPerTenant caps the Monitoring streams one tenant may hold open (0 = unlimited).
	MaxStreamsPerTenant int
}

// DefaultMonitoring is the sizing of the Monitoring stream: internal mechanics, not a setting. A poll reads informer caches (no API call),
// so one second is cheap; the caches hold the objects of all tenants (raise the agent resources with the number of labs).
var DefaultMonitoring = MonitoringConfig{
	JournalSize:         10000,
	JournalAge:          15 * time.Minute,
	PollInterval:        time.Second,
	SubscriberBuffer:    256,
	MaxStreamsPerTenant: 8,
}

// MTLSEnabled is whether callers must present a client certificate: always, but for the development switch AllowInsecure.
func (c *Config) MTLSEnabled() bool { return !c.AllowInsecure }

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, err
	}
	if c.RegistryAddr == "" {
		c.RegistryAddr = names.RegistryServiceAddr
	}
	if c.Cache.NodePrefix == "" {
		c.Cache.NodePrefix = names.RegistryNodePrefix
	}
	return c, nil
}
