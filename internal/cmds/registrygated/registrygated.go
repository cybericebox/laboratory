//go:build linux

// Package registrygated is the registry gate: a container of the registry pod, in front of zot. Every request to the registry (the relay
// of the nodes, the operator, the agent) reaches this process, and only this process reaches zot, which listens on the loopback of the
// pod. Every caller is authenticated by the projected ServiceAccount token it sends (verified with the API server) and decided by who it is
// and by what the cluster says it may have. docs/laboratory/specs/pools-release.md, section 2.4.
package registrygated

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/registrygate"
	"github.com/cybericebox/laboratory/pkg/config"
)

// Config comes from the environment the chart sets.
type Config struct {
	// TLSListen is the registry port of the pod (the Service's target): the only port the callers use, with the certificate and its key. The
	// token travels in every request, so there is no plain port. HealthListen is the port of the probes and the metrics.
	TLSListen    string `env:"GATE_TLS_LISTEN" envDefault:":5443"`
	HealthListen string `env:"GATE_HEALTH_LISTEN" envDefault:":5002"`
	TLSCertFile  string `env:"GATE_TLS_CERT_FILE" envDefault:"/etc/gate-tls/tls.crt"`
	TLSKeyFile   string `env:"GATE_TLS_KEY_FILE" envDefault:"/etc/gate-tls/tls.key"`
	// Zot is where zot listens: the loopback of the pod.
	Zot string `env:"GATE_ZOT_ADDR" envDefault:"127.0.0.1:5001"`

	// The ServiceAccounts the gate accepts, as <namespace>/<name>: the node-agent (the relay of a node), the operator and the agent.
	NodeAccount     string `env:"GATE_NODE_ACCOUNT" envDefault:"laboratory-system/laboratory-node-agent"`
	OperatorAccount string `env:"GATE_OPERATOR_ACCOUNT"`
	AgentAccount    string `env:"GATE_AGENT_ACCOUNT"`
	// TokenCacheTTL is the longest a verified token is remembered; BindSource requires the peer address of a node to be an address of the
	// node its pod is bound to (the internal load balancer must keep the client address).
	TokenCacheTTL time.Duration `env:"GATE_TOKEN_CACHE_TTL" envDefault:"5m"`
	BindSource    bool          `env:"GATE_BIND_SOURCE" envDefault:"true"`
	// APIStaleMax is the age of the view of the cluster past which a decision that needs it is answered 503.
	APIStaleMax time.Duration `env:"GATE_API_STALE_MAX" envDefault:"15m"`
	// PullGrace and GrantMaxAge bound how long a node may keep fetching what a manifest it was served lists.
	PullGrace     time.Duration `env:"GATE_PULL_GRACE" envDefault:"2m"`
	GrantMaxAge   time.Duration `env:"GATE_GRANT_MAX_AGE" envDefault:"1h"`
	RequireDigest bool          `env:"GATE_REQUIRE_DIGEST" envDefault:"false"`
	// Limits of 2.4.6a.
	GrantsPerNode     int `env:"GATE_GRANTS_PER_NODE" envDefault:"2000"`
	GrantMaxDigests   int `env:"GATE_GRANT_MAX_DIGESTS" envDefault:"512"`
	ManifestMaxBytes  int `env:"GATE_MANIFEST_MAX_BYTES" envDefault:"4194304"`
	TokenReviewConcur int `env:"GATE_TOKENREVIEW_CONCURRENCY" envDefault:"8"`
	// ImageHost is the host the nodes pull cache images from (<host>/c/...); empty: any host in front of /c/.
	ImageHost string `env:"GATE_IMAGE_HOST"`
	// AgentMaxFills is how many prewarm requests of the agent are served at once.
	AgentMaxFills int `env:"GATE_AGENT_MAX_FILLS" envDefault:"8"`

	// CacheEnabled turns the cache path (/v2/c/...) on. Off, that path answers 404.
	CacheEnabled bool `env:"GATE_CACHE_ENABLED" envDefault:"false"`
	// RegistriesFile is the list of upstream registries the cache may call (JSON), reloaded when it changes.
	RegistriesFile string `env:"GATE_REGISTRIES_FILE" envDefault:"/etc/gate/registries.json"`
	// AllowedRegistries is the laboratory's list of registries lab images may come from (registry.allowedRegistries): a cache request for
	// another registry is refused before any upstream call. Empty or "*": every registry of the list above.
	AllowedRegistries []string `env:"GATE_ALLOWED_REGISTRIES" envSeparator:","`
	// KeyFile is the secret of the credential hash; ZotUser and ZotPassFile are the gate's own account in zot.
	KeyFile     string `env:"GATE_KEY_FILE" envDefault:"/etc/gate-secret/key"`
	ZotUser     string `env:"GATE_ZOT_USER" envDefault:"gate"`
	ZotPassFile string `env:"GATE_ZOT_PASSWORD_FILE" envDefault:"/etc/gate-secret/password"`
	// AnonymousFillFile is an optional dockerconfigjson: a login per registry for filling images proved public.
	AnonymousFillFile string `env:"GATE_ANONYMOUS_FILL_FILE"`

	// The decision table (5.2).
	MaxAge       time.Duration `env:"GATE_DECISION_MAX_AGE" envDefault:"2m"`
	DeniedFor    time.Duration `env:"GATE_DENIED_FOR" envDefault:"30s"`
	StaleIfError time.Duration `env:"GATE_STALE_IF_ERROR" envDefault:"10m"`

	// DataDir is the volume of zot, mounted read-only to measure it; DiskHighWater is the percent above which a miss is not stored.
	DataDir       string `env:"GATE_DATA_DIR" envDefault:"/var/lib/registry"`
	DiskHighWater int    `env:"GATE_DISK_HIGH_WATER" envDefault:"90"`
	// UpstreamTimeout bounds the wait for an upstream answer (a blob body has no deadline of its own).
	UpstreamTimeout time.Duration `env:"GATE_UPSTREAM_TIMEOUT" envDefault:"30s"`

	// Snapshot pushes (devices.statePersistence.*): the limits the gate enforces for the nodes.
	SnapshotsEnabled bool          `env:"GATE_SNAPSHOTS_ENABLED" envDefault:"false"`
	MinPushInterval  time.Duration `env:"GATE_MIN_PUSH_INTERVAL" envDefault:"60s"`
	PushBudget       int64         `env:"GATE_PUSH_BUDGET_BYTES" envDefault:"2147483648"`
	PushBudgetWindow time.Duration `env:"GATE_PUSH_BUDGET_WINDOW" envDefault:"1h"`
	SupersededGrace  time.Duration `env:"GATE_SUPERSEDED_GRACE" envDefault:"2m"`
	// ExitMaxBytes is the most one exit push may carry (0: the device's write quota); ExitPushesPerHour the exit pushes a device gets per hour.
	ExitMaxBytes      int64   `env:"GATE_EXIT_MAX_BYTES" envDefault:"0"`
	ExitPushesPerHour int     `env:"GATE_EXIT_PUSHES_PER_HOUR" envDefault:"6"`
	UploadsPerDevice  int     `env:"GATE_UPLOADS_PER_DEVICE" envDefault:"4"`
	UploadsPerNode    int     `env:"GATE_UPLOADS_PER_NODE" envDefault:"64"`
	RegistryReserve   float64 `env:"GATE_REGISTRY_RESERVE" envDefault:"0.1"`
	TenantQuota       int64   `env:"GATE_TENANT_QUOTA_BYTES" envDefault:"10737418240"`
}

func fatal(log interface{ Error(error, string, ...any) }, err error, msg string) {
	log.Error(err, msg)
	os.Exit(1)
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// account reads "<namespace>/<name>"; an empty value is "no such caller".
func account(v, defNS, defName string) (registrygate.Account, error) {
	if v == "" {
		return registrygate.Account{Namespace: defNS, Name: defName}, nil
	}
	ns, name, ok := strings.Cut(v, "/")
	if !ok || ns == "" || name == "" {
		return registrygate.Account{}, fmt.Errorf("%q is not <namespace>/<name>", v)
	}
	return registrygate.Account{Namespace: ns, Name: name}, nil
}

// Run is the command "/node registry-gate".
func Run() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("registry-gate")
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		fatal(log, err, "load config")
	}
	gate, err := registrygate.New(cfg.Zot)
	if err != nil {
		fatal(log, err, "gate")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	metrics := registrygate.NewMetrics()
	gate.Metrics, gate.M = metrics.Handler(), metrics
	gate.TLSOnly = true

	nodeAcc, err := account(cfg.NodeAccount, names.SystemNamespace, "laboratory-node-agent")
	if err != nil {
		fatal(log, err, "GATE_NODE_ACCOUNT")
	}
	opAcc, err := account(cfg.OperatorAccount, names.SystemNamespace, names.OperatorServiceAccount)
	if err != nil {
		fatal(log, err, "GATE_OPERATOR_ACCOUNT")
	}
	agAcc, err := account(cfg.AgentAccount, names.AgentNamespace, names.AgentServiceAccount)
	if err != nil {
		fatal(log, err, "GATE_AGENT_ACCOUNT")
	}

	// the cluster view: watches, a heartbeat, and the TokenReview client
	schedule := registrygate.NewSchedule()
	secrets := registrygate.NewSecretView()
	restCfg := ctrl.GetConfigOrDie()
	src := &registrygate.KubeSources{Config: restCfg, Schedule: schedule, Secrets: secrets, OnError: func(err error) { log.Error(err, "the API server did not answer the heartbeat") }}
	cs, err := src.Clientset()
	if err != nil {
		fatal(log, err, "the Kubernetes client")
	}
	go func() {
		if err := src.Run(ctx); err != nil {
			fatal(log, err, "the cluster view stopped")
		}
	}()
	gate.Auth = &registrygate.Authenticator{
		Reviewer: registrygate.KubeReviewer{Client: cs}, Accounts: registrygate.Accounts{Node: nodeAcc, Operator: opAcc, Agent: agAcc},
		Schedule: schedule, CacheTTL: cfg.TokenCacheTTL, MaxStale: cfg.APIStaleMax, BindSource: cfg.BindSource, MaxReviews: cfg.TokenReviewConcur,
	}

	pass, err := readSecret(cfg.ZotPassFile)
	if err != nil || pass == "" {
		fatal(log, err, "read the zot password")
	}
	zot := registrygate.NewZot(cfg.Zot, cfg.ZotUser, pass)
	disk := &registrygate.DiskWatch{Path: cfg.DataDir, HighWater: cfg.DiskHighWater}

	if cfg.CacheEnabled {
		gate.Cache = buildCache(ctx, cfg, metrics, log, zot, disk, schedule, secrets)
	}
	if cfg.SnapshotsEnabled {
		gate.Lab = buildLab(ctx, cfg, metrics, log, zot, disk, schedule)
		gate.Lab.Cache = gate.Cache // a mount is allowed from the image the device runs (its grant), which the cache holds
	}

	servers := []*http.Server{
		registrygate.NewServer(cfg.TLSListen, gate),
		registrygate.NewServer(cfg.HealthListen, gate.HealthHandler()),
	}
	errs := make(chan error, len(servers))
	for i, s := range servers {
		tlsPort := i == 0
		go func() {
			if tlsPort {
				ln, err := registrygate.NewDeadlineListener(s.Addr, 10*time.Second)
				if err != nil {
					errs <- err
					return
				}
				errs <- s.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
				return
			}
			errs <- s.ListenAndServe()
		}()
	}
	log.Info("registry gate", "tls", cfg.TLSListen, "health", cfg.HealthListen, "zot", cfg.Zot, "cache", cfg.CacheEnabled, "snapshots", cfg.SnapshotsEnabled)
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			fatal(log, err, "serve")
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
}

type logger interface {
	Error(error, string, ...any)
	Info(string, ...any)
}

func buildLab(ctx context.Context, cfg Config, m *registrygate.Metrics, log logger, zot *registrygate.Zot, disk *registrygate.DiskWatch, sch *registrygate.Schedule) *registrygate.LabGate {
	lab := registrygate.NewLabGate(zot.Base, cfg.ZotUser, zot.Pass)
	lab.Schedule, lab.Store, lab.M, lab.MaxStale = sch, zot, m, cfg.APIStaleMax
	lab.Free = disk.FreeFraction
	lab.Limits = registrygate.PushLimits{
		MinInterval: cfg.MinPushInterval, Budget: cfg.PushBudget, BudgetWindow: cfg.PushBudgetWindow, Reserve: cfg.RegistryReserve, TenantQuota: cfg.TenantQuota,
		ExitMaxBytes: cfg.ExitMaxBytes, ExitPushesPerHour: cfg.ExitPushesPerHour, UploadsPerDevice: cfg.UploadsPerDevice, UploadsPerNode: cfg.UploadsPerNode,
	}
	lab.Meter = &registrygate.ZotMeter{Store: zot, Schedule: sch}
	lab.Super = &registrygate.Superseder{Store: zot, Grace: cfg.SupersededGrace, OnError: func(repo string, err error) { log.Error(err, "delete a superseded snapshot", "repository", repo) }}
	go lab.Super.Run(ctx, 10*time.Second)
	return lab
}

func buildCache(ctx context.Context, cfg Config, m *registrygate.Metrics, log logger, zot *registrygate.Zot, disk *registrygate.DiskWatch, sch *registrygate.Schedule, secrets *registrygate.SecretView) *registrygate.Cache {
	key, err := readSecret(cfg.KeyFile)
	if err != nil || key == "" {
		fatal(log, err, "read the credential hash key")
	}
	regs := &registrygate.RegistrySet{}
	if err := regs.Watch(ctx, cfg.RegistriesFile, 10*time.Second, func(err error) {
		log.Error(err, "the list of registries is not reloaded; the old one stays in force")
	}); err != nil {
		fatal(log, err, "load the list of registries")
	}
	store := registrygate.NewStore(registrygate.Timing{MaxAge: cfg.MaxAge, DeniedFor: cfg.DeniedFor, StaleIfError: cfg.StaleIfError})
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				store.Forget()
			}
		}
	}()
	c := registrygate.NewCache(regs, []byte(key), registrygate.NewUpstream(cfg.UpstreamTimeout), zot, store, m)
	c.Allowed = cfg.AllowedRegistries
	c.DiskFull = disk.Full
	c.Schedule, c.Creds, c.MaxStale = sch, secrets, cfg.APIStaleMax
	c.Grants = &registrygate.Grants{Grace: cfg.PullGrace, MaxAge: cfg.GrantMaxAge, PerNode: cfg.GrantsPerNode, MaxDigests: cfg.GrantMaxDigests}
	c.ManifestMaxBytes = cfg.ManifestMaxBytes
	go c.Grants.Run(ctx, time.Minute)
	c.RequireDigest, c.AgentMaxFills, c.ImageHost = cfg.RequireDigest, cfg.AgentMaxFills, cfg.ImageHost
	if cfg.AnonymousFillFile != "" {
		data, err := os.ReadFile(cfg.AnonymousFillFile)
		if err != nil {
			fatal(log, err, "read the anonymous fill credentials")
		}
		var list []registrygate.Registry
		for _, n := range regs.Names() {
			if r, ok := regs.Get(n); ok {
				list = append(list, r)
			}
		}
		if c.Fill, err = registrygate.ParseAnonymousFill(data, list); err != nil {
			fatal(log, err, "parse the anonymous fill credentials")
		}
	}
	return c
}
