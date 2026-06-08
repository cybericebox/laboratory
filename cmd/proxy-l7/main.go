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
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	proxy "github.com/cybericebox/laboratory/internal/proxy"
	"github.com/cybericebox/laboratory/internal/proxy/l7"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("proxy-l7")

	cfg, err := proxy.LoadL7Config()
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

	keyWatcher, err := proxy.NewKeyWatcher(cfg.JWTPublicKeyPath)
	if err != nil {
		log.Error(err, "init JWT key watcher")
		os.Exit(1)
	}

	handler := l7.NewHandler(keyWatcher.Key, cfg.BaseDomain, cfg.CookieName,
		l7.ServiceResolver(svcResolver))

	certWatcher, err := certwatcher.New(cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		log.Error(err, "init TLS cert watcher")
		os.Exit(1)
	}

	httpsSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		TLSConfig: &tls.Config{
			GetCertificate: certWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		},
	}

	for _, r := range []struct {
		runnable manager.Runnable
		name     string
	}{
		{certWatcher, "cert-watcher"},
		{keyWatcher, "key-watcher"},
		{manager.RunnableFunc(func(ctx context.Context) error {
			go func() {
				<-ctx.Done()
				_ = httpsSrv.Shutdown(context.Background())
			}()
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		}), "https-server"},
	} {
		if err := mgr.Add(r.runnable); err != nil {
			log.Error(err, "add runnable", "name", r.name)
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log.Info("starting l7 proxy", "listen", cfg.Listen)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}
