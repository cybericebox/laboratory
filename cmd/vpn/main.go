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
	log := ctrl.Log.WithName("vpn")

	cfg, err := loadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	wg, err := newWGManager(cfg.WGInterface)
	if err != nil {
		log.Error(err, "init WG manager")
		os.Exit(1)
	}
	defer wg.Close()

	if err := wg.Init(cfg.PrivateKey, cfg.ListenPort); err != nil {
		log.Error(err, "init WireGuard interface")
		os.Exit(1)
	}

	// Assign client-subnet gateway IP (e.g. 10.8.0.1/24) to wg0 so clients can
	// reach the VPN server itself and so the kernel routes traffic for the
	// client subnet via wg0. wgctrl only configures crypto/port; netlink does
	// addressing.
	gwCIDR := firstHostCIDR(cfg.ClientSubnet)
	if err := assignIfaceIP(cfg.WGInterface, gwCIDR); err != nil {
		log.Error(err, "assign gateway IP on wg interface", "iface", cfg.WGInterface, "cidr", gwCIDR)
		os.Exit(1)
	}

	ipt, err := newIPTablesManager(cfg.ClientSubnet)
	if err != nil {
		log.Error(err, "init iptables manager")
		os.Exit(1)
	}
	if err := ipt.SetupForwardPolicy(); err != nil {
		log.Error(err, "setup FORWARD policy")
		os.Exit(1)
	}
	defer ipt.Cleanup()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				cfg.Namespace: {},
			},
		},
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	if err := (&LabGroupClientReconciler{
		Client: mgr.GetClient(),
		WG:     wg,
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup LabGroupClientReconciler")
		os.Exit(1)
	}

	if err := (&LabVPNReconciler{
		Client: mgr.GetClient(),
		WG:     wg,
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup LabVPNReconciler")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go runStats(ctx, mgr.GetClient(), wg, cfg)

	log.Info("starting VPN manager", "namespace", cfg.Namespace, "interface", cfg.WGInterface)
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "manager error: %v\n", err)
		os.Exit(1)
	}
}
