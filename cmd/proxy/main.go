package main

import (
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/proxy/demux"
	"github.com/cybericebox/laboratory/internal/proxy/l7"
	proxy "github.com/cybericebox/laboratory/internal/proxy"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("proxy")

	cfg, err := proxy.LoadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	// Cluster-singleton: watch all namespaces (no DefaultNamespaces restriction).
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	table := demux.NewTable()
	ct := demux.NewConnTrack()

	if err := (&demux.LabGroupWatcher{
		Client:         mgr.GetClient(),
		Table:          table,
		VPNServicePort: cfg.WG.VPNServicePort,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup LabGroupWatcher")
		os.Exit(1)
	}

	// Service resolver: direct Get by name — informer cache, no List needed.
	svcResolver := func(task, namespace string) (string, error) {
		var svc corev1.Service
		if err := mgr.GetClient().Get(context.Background(),
			types.NamespacedName{Name: task, Namespace: namespace}, &svc,
		); err != nil {
			return "", fmt.Errorf("service %s not found in %s: %w", task, namespace, err)
		}
		if len(svc.Spec.Ports) == 0 {
			return "", fmt.Errorf("service %s has no ports", task)
		}
		proto := svc.Spec.Ports[0].Name
		if proto == "" {
			proto = "http"
		}
		return proto, nil
	}

	jwtPubKey, err := cfg.L7.ParsedJWTPublicKey()
	if err != nil {
		log.Error(err, "parse JWT public key")
		os.Exit(1)
	}

	handler := l7.NewHandler(jwtPubKey, cfg.L7.BaseDomain, cfg.L7.CookieName,
		l7.ServiceResolver(svcResolver))

	// certwatcher reloads the wildcard cert when cert-manager renews the
	// underlying Secret — no restart required. Falls back to a load error if
	// the files don't exist on boot.
	certWatcher, err := certwatcher.New(cfg.L7.TLSCertPath, cfg.L7.TLSKeyPath)
	if err != nil {
		log.Error(err, "init TLS cert watcher")
		os.Exit(1)
	}
	httpsSrv := &http.Server{
		Addr:    cfg.L7.Listen,
		Handler: handler,
		TLSConfig: &tls.Config{
			GetCertificate: certWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		},
	}

	dmx, err := demux.New(cfg.WG.ListenAddr, cfg.WG.ExternalInterface, table, ct)
	if err != nil {
		log.Error(err, "create demux")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	stop := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stop)
		_ = httpsSrv.Shutdown(context.Background())
	}()

	go ct.RunTTLCleanup(stop)
	go dmx.Run(stop)
	go func() {
		if err := certWatcher.Start(ctx); err != nil {
			log.Error(err, "cert watcher error")
		}
	}()
	go func() {
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Error(err, "HTTPS server error")
		}
	}()

	log.Info("starting proxy", "https", cfg.L7.Listen, "udp", cfg.WG.ListenAddr)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}
