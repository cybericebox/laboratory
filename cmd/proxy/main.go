package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/cmd/proxy/demux"
	"github.com/cybericebox/laboratory/cmd/proxy/l7"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("proxy")

	cfg, err := loadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	// Cluster-singleton: watch all namespaces (no DefaultNamespaces restriction).
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	table := demux.NewTable()
	ct := demux.NewConnTrack()

	if err := (&demux.LabGroupWatcher{
		Client: mgr.GetClient(),
		Table:  table,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup LabGroupWatcher")
		os.Exit(1)
	}

	// Service resolver: look up Service by name in the labgroup namespace.
	svcResolver := func(task, namespace string) (string, error) {
		var svcList corev1.ServiceList
		if err := mgr.GetClient().List(context.Background(), &svcList,
			client.InNamespace(namespace),
		); err != nil {
			return "", fmt.Errorf("list services in %s: %w", namespace, err)
		}
		for _, svc := range svcList.Items {
			if svc.Name == task {
				if len(svc.Spec.Ports) == 0 {
					return "", fmt.Errorf("service %s has no ports", task)
				}
				proto := svc.Spec.Ports[0].Name
				if proto == "" {
					proto = "http"
				}
				return proto, nil
			}
		}
		return "", fmt.Errorf("service %s not found in %s", task, namespace)
	}

	handler := l7.NewHandler(cfg.Ed25519PubKey, cfg.BaseDomain, cfg.CookieName,
		l7.ServiceResolver(svcResolver))

	tlsCert, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		log.Error(err, "load TLS cert")
		os.Exit(1)
	}
	httpsSrv := &http.Server{
		Addr:    cfg.ListenHTTPS,
		Handler: handler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	dmx, err := demux.New(cfg.UDPListenAddr, table, ct)
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
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Error(err, "HTTPS server error")
		}
	}()

	log.Info("starting proxy", "https", cfg.ListenHTTPS, "udp", cfg.UDPListenAddr)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}
