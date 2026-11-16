//go:build linux

// Package registrygated is the registry gate: a container of the registry pod, in front of zot. Every request to the registry
// (the relay of the nodes, the operator, the agent) reaches this process, and only this process reaches zot, which listens on the
// loopback of the pod. docs/research/2026-11-16-zot-tenant-cache.md.
package registrygated

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cybericebox/laboratory/internal/registrygate"
	"github.com/cybericebox/laboratory/pkg/config"
)

// Config comes from the environment the chart sets.
type Config struct {
	// Listen is the registry port of the pod (the Service's target); HealthListen the port of the probes and the metrics.
	Listen       string `env:"GATE_LISTEN" envDefault:":5000"`
	HealthListen string `env:"GATE_HEALTH_LISTEN" envDefault:":5002"`
	// Zot is where zot listens: the loopback of the pod.
	Zot string `env:"GATE_ZOT_ADDR" envDefault:"127.0.0.1:5001"`

	// TLSListen, with the certificate and its private key, is the TLS port of the gate: the cache path is served there only, because the
	// pull's credentials travel in it from the relay of a node.
	TLSListen   string `env:"GATE_TLS_LISTEN"`
	TLSCertFile string `env:"GATE_TLS_CERT_FILE" envDefault:"/etc/gate-tls/tls.crt"`
	TLSKeyFile  string `env:"GATE_TLS_KEY_FILE" envDefault:"/etc/gate-tls/tls.key"`

	// CacheEnabled turns the cache path (/v2/c/...) on. Off, that path answers 404 and the rest is the pass-through.
	CacheEnabled bool `env:"GATE_CACHE_ENABLED" envDefault:"false"`
	// RegistriesFile is the list of upstream registries the cache may call (JSON), reloaded when it changes.
	RegistriesFile string `env:"GATE_REGISTRIES_FILE" envDefault:"/etc/gate/registries.json"`
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
}

func fatal(log interface{ Error(error, string, ...any) }, err error, msg string) {
	log.Error(err, msg)
	os.Exit(1)
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
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
	gate.Metrics = metrics.Handler()
	if cfg.CacheEnabled {
		gate.Cache = buildCache(ctx, cfg, metrics, log)
		gate.CacheTLSOnly = cfg.TLSListen != ""
	}

	servers := []*http.Server{
		registrygate.NewServer(cfg.Listen, gate),
		registrygate.NewServer(cfg.HealthListen, gate.HealthHandler()),
	}
	if cfg.TLSListen != "" {
		servers = append(servers, registrygate.NewServer(cfg.TLSListen, gate))
	}
	errs := make(chan error, len(servers))
	for i, s := range servers {
		tlsPort := i == 2
		go func() {
			if tlsPort {
				errs <- s.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
				return
			}
			errs <- s.ListenAndServe()
		}()
	}
	log.Info("registry gate", "listen", cfg.Listen, "health", cfg.HealthListen, "zot", cfg.Zot, "cache", cfg.CacheEnabled)
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

func buildCache(ctx context.Context, cfg Config, m *registrygate.Metrics, log interface {
	Error(error, string, ...any)
	Info(string, ...any)
}) http.Handler {
	key, err := readSecret(cfg.KeyFile)
	if err != nil || key == "" {
		fatal(log, err, "read the credential hash key")
	}
	pass, err := readSecret(cfg.ZotPassFile)
	if err != nil || pass == "" {
		fatal(log, err, "read the zot password")
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
	c := registrygate.NewCache(regs, []byte(key), registrygate.NewUpstream(cfg.UpstreamTimeout),
		registrygate.NewZot(cfg.Zot, cfg.ZotUser, pass), store, m)
	disk := &registrygate.DiskWatch{Path: cfg.DataDir, HighWater: cfg.DiskHighWater}
	c.DiskFull = disk.Full
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
