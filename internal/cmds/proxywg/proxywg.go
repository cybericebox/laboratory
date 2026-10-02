//go:build linux

package proxywg

import (
	"github.com/cybericebox/laboratory/internal/errorlog"
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	table := demux.NewTable()
	limits := demux.Limits{
		MaxEntries: cfg.MaxEntries, MaxEntriesPerSource: cfg.MaxEntriesPerSource,
		HandshakeRate: cfg.HandshakeRate, HandshakeBurst: cfg.HandshakeBurst, MissRate: cfg.MissRate, MissBurst: cfg.MissBurst,
		RoamInterval: cfg.RoamInterval, MaxSources: cfg.MaxSources,
	}
	ct := demux.NewConnTrackWithLimits(limits)

	if err := (&demux.LabGroupWatcher{
		Client:         mgr.GetClient(),
		Table:          table,
		VPNServicePort: cfg.VPNServicePort,
	}).SetupWithManager(mgr); err != nil {
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

	go func() {
		<-ctx.Done()
		close(stop)
	}()
	go ct.RunTTLCleanup(stop)
	go dmx.Run(stop)

	log.Info("starting wg proxy", "udp", cfg.ListenAddr)
	errorlog.Start(ctx, journal, ctrl.GetConfigOrDie(), errorlog.Namespace("laboratory-system"), ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}
