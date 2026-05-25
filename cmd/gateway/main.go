//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("gateway")

	cfg, err := loadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	ipt, err := newIPTablesManager(cfg.ExternalInterface)
	if err != nil {
		log.Error(err, "init iptables")
		os.Exit(1)
	}

	dhcp := newDHCPManager()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}},
		},
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	if err := (&LabGatewayReconciler{
		Client: mgr.GetClient(),
		DHCP:   dhcp,
		IPT:    ipt,
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup reconciler")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log.Info("starting gateway manager", "namespace", cfg.Namespace)
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "manager error: %v\n", err)
		os.Exit(1)
	}
}
