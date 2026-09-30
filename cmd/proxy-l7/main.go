package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	
	_ "github.com/cybericebox/laboratory/pkg/runtime"
	
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
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/proxy"
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
	
	mgr, err := ctrl.NewManager(
		ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme:  scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		},
	)
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	
	svcResolver := func(task, namespace string) (string, error) {
		var svc corev1.Service
		if err := mgr.GetClient().Get(
			context.Background(),
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
	
	mode := l7.TokenMode(cfg.TokenMode)
	if !mode.Valid() {
		log.Error(fmt.Errorf("unknown TOKEN_MODE %q", cfg.TokenMode), "load config")
		os.Exit(1)
	}
	instance := cfg.Instance
	if instance == "" {
		instance, _ = os.Hostname()
	}
	started := time.Now()
	meter := l7.NewMeter(fmt.Sprintf("%s-%d", instance, started.UnixNano()), started)

	// A request is attributed to a lab through the labels the operator puts on
	// the web Service of every exposed device. The host <device>-<labid> carries
	// the base36 id of the Lab UID; the Service is looked up in the namespace of
	// the token's own group, so a token can only reach labs its group owns, and
	// the id in the host must match the one on the Service.
	attribute := func(task, groupID string) (string, bool) {
		hostID, ok := names.LabIDFromWebHostLabel(task)
		if !ok {
			return "", false
		}
		var svc corev1.Service
		key := types.NamespacedName{Name: task, Namespace: laboratoryv1alpha1.LabGroupNamespace(groupID)}
		if err := mgr.GetClient().Get(context.Background(), key, &svc); err != nil {
			return "", false
		}
		lab := svc.Labels[names.LabelLab]
		return lab, lab != "" && svc.Labels[names.LabelLabID] == hostID
	}
	// The group access policy is the same one the VPN enforces. In per-user mode
	// a lab is reachable only while the policy allows this participant; in mixed
	// mode only explicit denies apply.
	authorize := func(groupID, subject, lab string) bool {
		var policy laboratoryv1alpha1.LabGroupAccessPolicy
		key := types.NamespacedName{Name: names.LabGroupAccessPolicyName, Namespace: laboratoryv1alpha1.LabGroupNamespace(groupID)}
		if err := mgr.GetClient().Get(context.Background(), key, &policy); err != nil {
			return mode != l7.ModePerUser
		}
		return l7.PolicyAllows(policy.Spec.Rules, l7.ClientName(subject), lab, mode == l7.ModePerUser)
	}
	handler := l7.NewHandler(
		keyWatcher.Key, cfg.BaseDomain, cfg.CookieName,
		l7.ServiceResolver(svcResolver),
	).WithTokenMode(mode).WithAccounting(meter, attribute).WithAuthorizer(authorize)

	reports := &l7.ReportWriter{
		Reader: mgr.GetAPIReader(), Writer: mgr.GetClient(), Meter: meter, Instance: instance,
		Namespaces: func(ctx context.Context) []string {
			var groups laboratoryv1alpha1.LabGroupList
			if err := mgr.GetClient().List(ctx, &groups); err != nil {
				return nil
			}
			out := make([]string, 0, len(groups.Items))
			for i := range groups.Items {
				if ns := groups.Items[i].Status.Namespace; ns != "" {
					out = append(out, ns)
				}
			}
			return out
		},
	}
	
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
		{
			manager.RunnableFunc(func(ctx context.Context) error {
				reports.Run(ctx, cfg.ReportInterval, func(err error) { log.Error(err, "publish traffic report") })
				return nil
			}), "traffic-reports",
		},
		{certWatcher, "cert-watcher"},
		{keyWatcher, "key-watcher"},
		{
			manager.RunnableFunc(
				func(ctx context.Context) error {
					go func() {
						<-ctx.Done()
						_ = httpsSrv.Shutdown(context.Background())
					}()
					if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
						return err
					}
					return nil
				},
			), "https-server",
		},
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
