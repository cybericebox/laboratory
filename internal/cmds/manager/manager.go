/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manager

import (
	"github.com/cybericebox/laboratory/internal/errorlog"
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/cybericebox/laboratory/internal/health"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/admissioncheck"
	laboratorycontroller "github.com/cybericebox/laboratory/internal/controller/laboratory"
	"github.com/cybericebox/laboratory/internal/crdcheck"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/operator"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(allocationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func Run() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	journalLog, journal := errorlog.Setup("operator", errorlog.Instance(), zap.New(zap.UseFlagOptions(&opts)))
	ctrl.SetLogger(journalLog)

	cfg, err := operator.LoadConfig()
	if err != nil {
		setupLog.Error(err, "load config")
		os.Exit(1)
	}

	labNodeSelector, labTolerations, err := operator.ParseLabScheduling(cfg)
	if err != nil {
		setupLog.Error(err, "parse lab scheduling config")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// The scheduler writes the queue status of many objects; the default client-side
	// rate limit (20 QPS, burst 30) would also slow every reconciler.
	restCfg := ctrl.GetConfigOrDie()
	restCfg.QPS = 60
	restCfg.Burst = 120

	// The operator and the agent need the CRDs of this release (the enrollment epoch among them): refuse to run on older ones.
	if dc, err := discovery.NewDiscoveryClientForConfig(restCfg); err != nil {
		setupLog.Error(err, "unable to build the discovery client")
		os.Exit(1)
	} else if err := crdcheck.Wait(context.Background(), crdcheck.FromDiscovery(dc), 60*time.Second, func(err error) { setupLog.Info("waiting for the CRDs", "reason", err.Error()) }); err != nil {
		setupLog.Error(err, "the CRDs are older than this release")
		os.Exit(1)
	}

	if cfg.RequireAdmissionPolicy {
		// The operator is confined by admission policies the chart installs; refuse to run when they are not enforced.
		probe, err := client.New(restCfg, client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "unable to build the client that checks the admission policies")
			os.Exit(1)
		}
		if err := admissioncheck.Wait(context.Background(), probe, cfg.AdmissionPolicyTimeout, 5*time.Second, func(err error) {
			setupLog.Info("the admission policies are not (yet) enforced", "reason", err.Error())
		}); err != nil {
			setupLog.Error(err, "the operator's admission policies are not enforced: refusing to run (set operator.admissionPolicy.enabled=false in the chart to accept that on purpose)")
			os.Exit(1)
		}
		setupLog.Info("the admission policies are enforced")
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2b172d9d.cybericebox.com",
		// Secrets are read straight from the API server, not through an informer: an informer would need list and
		// watch on every Secret of the cluster, and the operator has no such permission (only in its own namespaces).
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Image cache: lab images of labs created while it is on are pulled through it.
	mirror := cfg.Cache.Rewriter()
	var resolver imagecache.Resolver

	if cfg.Cache.Enabled {
		// Tags are pinned to digests asked of the upstream registries directly, with
		// the operator's pull secrets.
		resolver = &imagecache.RegistryResolver{
			Keychain: &laboratorycontroller.PullKeychain{Reader: mgr.GetAPIReader(), Namespace: names.SystemNamespace, Names: cfg.ImagePullSecrets},
			TTL:      cfg.Cache.PinTTL,
			// One node architecture: pin the platform manifest, so the cache fetches
			// that image and not every architecture of an index.
			Platforms: laboratorycontroller.NodePlatforms(mgr.GetAPIReader(), labNodeSelector),
		}
	}

	statePolicy, stateRegistry, err := laboratorycontroller.SetupState(cfg.State)
	if err != nil {
		setupLog.Error(err, "state persistence config")
		os.Exit(1)
	}

	if err = (&laboratorycontroller.LabGroupReconciler{
		Scheduled:         cfg.SchedulerEnabled,
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorderFor("labgroup"),
		PublicVPNEndpoint: cfg.PublicVPNEndpoint,
		VPNServicePort:    cfg.VPNServicePort,
		VPNBaseNetwork:    cfg.VPNBaseNetwork,
		InetBaseNetwork:   cfg.InetBaseNetwork,
		VPNImage:          cfg.VPNImage,
		Mirror:            mirror,
		Resolver:          resolver,
		GatewayImage:      cfg.GatewayImage,
		GroupPods:         cfg.GroupPods,
		PriorityClass:     cfg.GroupPriorityClass,
		SchedulerName:     cfg.LabSchedulerName,
		SupportEmail:      cfg.SupportEmail,
		LabNodeSelector:   labNodeSelector,
		LabTolerations:    labTolerations,
		AgentEnabled:      cfg.AgentEnabled,
		ProxyEnabled:      cfg.ProxyEnabled,
		AgentSA: types.NamespacedName{
			Namespace: cfg.AgentServiceNamespace,
			Name:      cfg.AgentServiceAccount,
		},
		NetworkPolicyEnabled: cfg.NetworkPolicyEnabled,
		VPNStatsInterval:     cfg.VPNStatsInterval,
		OperatorSA:           types.NamespacedName{Namespace: cfg.OperatorNamespace, Name: cfg.OperatorServiceAccount},
		ImagePullSecrets:     cfg.ImagePullSecrets,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LabGroup")
		os.Exit(1)
	}
	if err = (&laboratorycontroller.LabGroupClientReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorderFor("labgroupclient"),
		VPNBaseNetwork: cfg.VPNBaseNetwork,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LabGroupClient")
		os.Exit(1)
	}
	if err = (&laboratorycontroller.LabReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Recorder:         mgr.GetEventRecorderFor("lab"),
		BaseDomain:       cfg.BaseDomain,
		ProxySourceCIDRs: cfg.ProxySourceCIDRs,
		VPNBaseNetwork:   cfg.VPNBaseNetwork,
		InetBaseNetwork:  cfg.InetBaseNetwork,
		State:            statePolicy,
		Mirror:           mirror,
		Resolver:         resolver,
		NetConfigImage:   cfg.NetConfigImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Lab")
		os.Exit(1)
	}
	if cfg.SchedulerEnabled {
		if err = mgr.Add(&laboratorycontroller.Scheduler{
			Mirror:   mirror,
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorderFor("scheduler"),
			Config: laboratorycontroller.SchedulerConfig{
				MaxPods:                cfg.SchedulerMaxPods,
				StartupTimeout:         cfg.SchedulerStartupTimeout,
				RestartThreshold:       cfg.SchedulerRestartThreshold,
				PlatformReservePercent: cfg.SchedulerPlatformReservePercent,
				PlatformReserveNode: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cfg.SchedulerPlatformReserveCPU),
					corev1.ResourceMemory: resource.MustParse(cfg.SchedulerPlatformReserveMemory),
				},
				ResourceCheck:  cfg.SchedulerResourceCheck,
				Prepull:        cfg.SchedulerPrepull,
				PrepullTimeout: cfg.SchedulerPrepullTimeout,
			},
			Defaults:        laboratorycontroller.DeviceDefaults{CPU: cfg.DeviceDefaultCPU, Memory: cfg.DeviceDefaultMemory, MaxCPU: cfg.DeviceMaxCPU, MaxMemory: cfg.DeviceMaxMemory},
			GroupPods:       cfg.GroupPods,
			LabNodeSelector: labNodeSelector,
			LabTolerations:  labTolerations,
		}); err != nil {
			setupLog.Error(err, "unable to add the scheduler")
			os.Exit(1)
		}
	}
	deviceReconciler := &laboratorycontroller.DeviceReconciler{
		Scheduled:        cfg.SchedulerEnabled,
		MirrorRegistries: mirror.Registries,
		Reader:           mgr.GetAPIReader(),
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		LabNodeSelector:  labNodeSelector,
		LabTolerations:   labTolerations,
		PriorityClass:    cfg.DevicePriorityClass,
		SchedulerName:    cfg.LabSchedulerName,
		NetConfigImage:   cfg.NetConfigImage,
		Defaults:         laboratorycontroller.DeviceDefaults{CPU: cfg.DeviceDefaultCPU, Memory: cfg.DeviceDefaultMemory, MaxCPU: cfg.DeviceMaxCPU, MaxMemory: cfg.DeviceMaxMemory},
		Security: laboratorycontroller.PodSecurity{
			UserNamespaces: cfg.DeviceUserNamespaces, EphemeralStorage: cfg.DeviceEphemeralStorage,
		},
	}
	if stateRegistry != nil {
		deviceReconciler.Registry = stateRegistry
	}
	deviceReconciler.NodeLossForceDeleteAfter = cfg.State.NodeLossForceDeleteAfter
	if cfg.State.NodeLossForceDeleteAfter == 0 {
		deviceReconciler.NodeLossForceDeleteAfter = -1 // "0" turns the force-delete off
	}
	if err = deviceReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Device")
		os.Exit(1)
	}
	if err = (&laboratorycontroller.ConnectionReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("connection"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Connection")
		os.Exit(1)
	}
	if err = (&laboratorycontroller.TenantReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		TTL:    cfg.TenantEnrollmentTTL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tenant")
		os.Exit(1)
	}
	if stateRegistry != nil {
		if err = mgr.Add(&laboratorycontroller.RetentionSweeper{
			Client:    mgr.GetClient(),
			Reader:    mgr.GetAPIReader(),
			Registry:  stateRegistry,
			Retention: cfg.State.Retention,
			Interval:  cfg.State.RetentionInterval,
			Namespace: names.SystemNamespace,
		}); err != nil {
			setupLog.Error(err, "unable to add snapshot retention sweep")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	// Ready means the controllers can read: the informer caches have synced.
	if err := mgr.AddReadyzCheck("caches", health.CacheSynced(mgr.GetCache())); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	ctx := ctrl.SetupSignalHandler()
	errorlog.Start(ctx, journal, restCfg, cfg.OperatorNamespace, ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
