package proxyl7

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cybericebox/laboratory/internal/errorlog"

	_ "github.com/cybericebox/laboratory/pkg/runtime"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/health"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/proxy"
	"github.com/cybericebox/laboratory/internal/proxy/l7"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func Run() {
	journalLog, journal := errorlog.Setup("l7-proxy", errorlog.Instance(), zap.New())
	ctrl.SetLogger(journalLog)
	log := ctrl.Log.WithName("proxy-l7")

	cfg, err := proxy.LoadL7Config()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(
		ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: cfg.HealthAddr,
			// Secrets are read for the tenants' access keys only: watch that namespace, nothing else.
			Cache: proxyCacheOptions(),
		},
	)
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	instance := cfg.Instance
	if instance == "" {
		instance, _ = os.Hostname()
	}
	started := time.Now()
	meter := l7.NewMeter(fmt.Sprintf("%s-%d", instance, started.UnixNano()), started)

	access := &l7.AccessReader{Reader: mgr.GetClient()}
	handler := l7.NewHandler(l7.SecretKeys(mgr.GetClient()), []byte(cfg.SessionSecret), cfg.BaseDomain, cfg.CookieName, nil).
		WithLimits(cfg.AccessTokenMaxTTL, cfg.SessionIdleTTL, cfg.SessionRenewBefore, cfg.SessionMaxTTL).
		WithAccounting(meter, nil).WithAccessReader(access).WithLiveMaxLifetime(cfg.LiveMaxLifetime).
		WithLiveCaps(l7.LiveCaps{PerClient: cfg.LivePerClient, PerGroup: cfg.LivePerGroup, Total: cfg.LiveTotal}).WithAuthRateLimit(cfg.AuthRate, cfg.AuthBurst)
	// Register all required informers before the manager starts: readiness must
	// wait for each cache used by authorization, rather than an empty cache set.
	if err := warmProxyCache(context.Background(), mgr.GetCache()); err != nil {
		log.Error(err, "register proxy cache")
		os.Exit(1)
	}

	reports := &l7.ReportWriter{
		Reader: mgr.GetAPIReader(), Writer: mgr.GetClient(), Meter: meter, Instance: instance,
		Namespaces: func(ctx context.Context) []string { return groupNamespaces(ctx, mgr.GetClient()) },
	}

	// Ready means the proxy really serves: the HTTPS listener is bound and the caches (groups, clients, policies, access keys) have synced.
	var listening health.Flag
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		log.Error(err, "add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("caches", health.CacheSynced(mgr.GetCache())); err != nil {
		log.Error(err, "add ready check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("listener", listening.Check("the HTTPS listener is not bound yet")); err != nil {
		log.Error(err, "add ready check")
		os.Exit(1)
	}

	certWatcher, err := certwatcher.New(cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		log.Error(err, "init TLS cert watcher")
		os.Exit(1)
	}

	httpsSrv := proxy.NewHTTPServer(cfg, handler, &tls.Config{
		GetCertificate: certWatcher.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	})

	for _, r := range []struct {
		runnable manager.Runnable
		name     string
	}{
		{certWatcher, "cert-watcher"},
		{manager.RunnableFunc(func(ctx context.Context) error {
			if !mgr.GetCache().WaitForCacheSync(ctx) {
				return nil
			}
			ln, err := net.Listen("tcp", httpsSrv.Addr)
			if err != nil {
				return err
			}
			defer ln.Close()
			return l7.RunLifecycle(ctx, httpsSrv, handler, reports, cfg.ReportInterval, cfg.LiveCheckInterval, func() error {
				listening.Set()
				return httpsSrv.ServeTLS(proxy.LimitListener(ln, cfg.MaxConnections), "", "")
			}, l7.LogReportFailure(log))
		}), "proxy-lifecycle"},
	} {
		if err := mgr.Add(r.runnable); err != nil {
			log.Error(err, "add runnable", "name", r.name)
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log.Info("starting l7 proxy", "listen", cfg.Listen)
	errorlog.Start(ctx, journal, ctrl.GetConfigOrDie(), errorlog.Namespace("laboratory-system"), ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}

func proxyCacheOptions() cache.Options {
	return cache.Options{ReaderFailOnMissingInformer: true, ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}:                           {Namespaces: map[string]cache.Config{names.AccessKeysNamespace: {}}, Transform: l7.CompactCacheObject},
		&corev1.Service{}:                          {Transform: l7.CompactCacheObject},
		&laboratoryv1alpha1.LabGroup{}:             {Transform: l7.CompactCacheObject},
		&laboratoryv1alpha1.LabGroupClient{}:       {Transform: l7.CompactCacheObject},
		&laboratoryv1alpha1.LabGroupAccessPolicy{}: {Transform: l7.CompactCacheObject},
	}}
}
func warmProxyCache(ctx context.Context, c cache.Cache) error {
	for _, object := range []client.Object{&corev1.Secret{}, &corev1.Service{}, &laboratoryv1alpha1.LabGroup{}, &laboratoryv1alpha1.LabGroupClient{}, &laboratoryv1alpha1.LabGroupAccessPolicy{}} {
		if _, err := c.GetInformer(ctx, object, cache.BlockUntilSynced(false)); err != nil {
			return err
		}
	}
	return nil
}

func groupNamespaces(ctx context.Context, reader client.Reader) []string {
	var groups laboratoryv1alpha1.LabGroupList
	if err := reader.List(ctx, &groups); err != nil {
		return nil
	}
	out := make([]string, 0, len(groups.Items))
	for i := range groups.Items {
		out = append(out, laboratoryv1alpha1.LabGroupNamespaceOf(&groups.Items[i]))
	}
	return out
}
