//go:build linux

package vpn

import (
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn"
	vpnreconciler "github.com/cybericebox/laboratory/internal/vpn/reconciler"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(allocationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func Run() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("vpn")

	cfg, err := vpn.LoadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	srv, err := vpn.InitServer(cfg)
	if err != nil {
		log.Error(err, "init VPN server")
		os.Exit(1)
	}
	defer srv.Cleanup()

	mgr, err := ctrl.NewManager(
		ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme: scheme,
			Cache: cache.Options{
				DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}},
			},
			Metrics:                server.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		},
	)
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := vpnreconciler.Setup(ctx, mgr, srv.WG, srv.IPT, cfg); err != nil {
		log.Error(err, "setup VPN reconcilers")
		os.Exit(1)
	}

	log.Info("starting VPN manager", "namespace", cfg.Namespace, "interface", cfg.WGInterface)
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "manager error: %v\n", err)
		os.Exit(1)
	}
}
