//go:build linux

package proxywg

import (
	"github.com/cybericebox/laboratory/internal/errorlog"
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/health"
	"github.com/cybericebox/laboratory/internal/names"
	proxy "github.com/cybericebox/laboratory/internal/proxy"
	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func Run() {
	journalLog, journal := errorlog.Setup("wg-demux", errorlog.Instance(), zap.New())
	ctrl.SetLogger(journalLog)
	log := ctrl.Log.WithName("proxy-wg")

	cfg, err := proxy.LoadWGConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: cfg.HealthAddr,
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	table := demux.NewTable()
	limits := demux.Limits{
		MaxEntries: cfg.MaxEntries, PartialTTL: cfg.PartialTTL,
		GlobalHandshakeRate: cfg.GlobalHandshakeRate, GlobalHandshakeBurst: cfg.GlobalHandshakeBurst,
		MissRate: cfg.MissRate, MissBurst: cfg.MissBurst, RoamInterval: cfg.RoamInterval,
		SessionRate: cfg.SessionRate, SessionBurst: cfg.SessionBurst,
		Readers: cfg.Readers,
	}
	ct := demux.NewConnTrackWithLimits(limits)

	watcher := &demux.LabGroupWatcher{
		Client:         mgr.GetClient(),
		Table:          table,
		VPNServicePort: names.WireGuardPort,
	}
	if err := watcher.SetupWithManager(mgr); err != nil {
		log.Error(err, "setup LabGroupWatcher")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	stop := make(chan struct{})

	dmx, err := demux.NewWithLimits(cfg.ListenAddr, table, ct, limits)
	if err != nil {
		log.Error(err, "create demux")
		os.Exit(1)
	}

	// Ready means the demux really serves: the UDP socket is bound (above), the caches have synced, and every group has been put in the
	// table (a handshake of a group that is not in it is dropped, and the client re-handshakes after a long wait).
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		log.Error(err, "add health check")
		os.Exit(1)
	}
	for name, check := range map[string]healthz.Checker{
		"caches": health.CacheSynced(mgr.GetCache()),
		"table":  health.Func("the demux table is not filled from the groups yet", watcher.Synced),
	} {
		if err := mgr.AddReadyzCheck(name, check); err != nil {
			log.Error(err, "add ready check")
			os.Exit(1)
		}
	}

	go func() {
		<-ctx.Done()
		close(stop)
	}()
	go ct.RunTTLCleanup(stop)
	go table.RunResolver(stop, 30*time.Second)
	go dmx.Run(stop)

	log.Info("starting wg proxy", "udp", cfg.ListenAddr)
	errorlog.Start(ctx, journal, ctrl.GetConfigOrDie(), errorlog.Namespace("laboratory-system"), ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}
