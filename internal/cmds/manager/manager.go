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
	"fmt"
	"os"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/cybericebox/laboratory/internal/health"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

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
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Leader election of the controller manager: only one active manager at a time. "+
			"On by default; --leader-elect=false for a single manager on a developer's machine.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
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

	// HTTP/2 stays off on the metrics server: it would make the manager vulnerable to the HTTP/2 Stream Cancellation and Rapid Reset
	// CVEs (https://github.com/advisories/GHSA-qppj-fm5r-hxr3, https://github.com/advisories/GHSA-4374-p667-p6c8). No webhook is
	// served (none is registered), so there is no webhook server to configure.
	tlsOpts := []func(*tls.Config){func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}}

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

	// The metrics server is off unless --metrics-bind-address is given; controller-runtime then makes a self-signed certificate.

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

	var serviceReleaseObserver laboratorycontroller.GroupServiceReleaseObserver
	if cfg.RuntimeObservation {
		serviceReleaseObserver = laboratorycontroller.NativeGroupServiceReleaseObserver{Client: mgr.GetClient()}
	}
	profilePolicy, err := cfg.Profiles.Policy()
	if err != nil {
		setupLog.Error(err, "device profiles config")
		os.Exit(1)
	}
	var profilePolicyOverheadCPU, profilePolicyOverheadMemory string
	if profilePolicy.MicroVM {
		profilePolicyOverheadCPU, profilePolicyOverheadMemory = cfg.Profiles.MicroVMOverheadCPU, cfg.Profiles.MicroVMOverheadMemory
	}
	if err = (&laboratorycontroller.LabGroupReconciler{
		Profiles:                    profilePolicy,
		Reader:                      mgr.GetAPIReader(),
		ServiceReleaseObserver:      serviceReleaseObserver,
		Scheduled:                   cfg.SchedulerEnabled,
		BandwidthEnabled:            cfg.Bandwidth.Enabled,
		Client:                      mgr.GetClient(),
		Scheme:                      mgr.GetScheme(),
		Recorder:                    mgr.GetEventRecorderFor("labgroup"),
		PublicVPNEndpoint:           cfg.PublicVPNEndpoint,
		WGDemuxMode:                 cfg.WGDemuxMode,
		VPNBaseNetwork:              cfg.VPNBaseNetwork,
		VPNFirewall:                 cfg.VPNFirewall,
		GatewayMaxConnectionsPerLab: cfg.GatewayMaxConnectionsPerLab, GatewayNewConnectionsPerSecondPerLab: cfg.GatewayNewConnectionsPerSecondPerLab,
		GatewayEgressEnv:        gatewayEgressEnv(cfg),
		InetBaseNetwork:         cfg.InetBaseNetwork,
		VPNImage:                cfg.VPNImage,
		Mirror:                  mirror,
		Resolver:                resolver,
		GatewayImage:            cfg.GatewayImage,
		GroupPods:               cfg.GroupPods,
		PriorityClass:           cfg.GroupPriorityClass,
		SchedulerName:           cfg.LabSchedulerName,
		TerminationGraceSeconds: cfg.LabPodTerminationGraceSeconds,
		SupportEmail:            cfg.SupportEmail,
		LabNodeSelector:         labNodeSelector,
		LabTolerations:          labTolerations,
		AgentEnabled:            cfg.AgentEnabled,
		ProxyEnabled:            cfg.ProxyEnabled,
		AgentSA: types.NamespacedName{
			Namespace: cfg.AgentServiceNamespace,
			Name:      cfg.AgentServiceAccount,
		},
		NetworkPolicyEnabled: cfg.NetworkPolicyEnabled,
		OperatorSA:           types.NamespacedName{Namespace: cfg.OperatorNamespace, Name: cfg.OperatorServiceAccount},
		VPNStatsInterval:     cfg.VPNStatsInterval,
		ImagePullSecrets:     cfg.ImagePullSecrets,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LabGroup")
		os.Exit(1)
	}
	if err = (&laboratorycontroller.LabGroupClientReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorderFor("labgroupclient"),
		Reader:         mgr.GetAPIReader(),
		VPNBaseNetwork: cfg.VPNBaseNetwork,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LabGroupClient")
		os.Exit(1)
	}
	if cfg.Bandwidth.Enabled {
		bw := cfg.Bandwidth.Config()
		if err := bw.Validate(); err != nil {
			setupLog.Error(err, "bandwidth config")
			os.Exit(1)
		}
		if err = (&laboratorycontroller.BandwidthReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Config: bw, EntryNamespaces: cfg.BandwidthEntryNamespaces}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Bandwidth")
			os.Exit(1)
		}
	}
	vniRetirer := &laboratorycontroller.VNIRetirer{Client: mgr.GetClient(), Interval: cfg.VNIRetireFlush}
	if err = mgr.Add(vniRetirer); err != nil {
		setupLog.Error(err, "unable to add the VNI retirer")
		os.Exit(1)
	}
	if err = (&laboratorycontroller.LabReconciler{
		VNI:                       vniRetirer,
		Defaults:                  laboratorycontroller.DeviceDefaults{CPU: cfg.DeviceDefaultCPU, Memory: cfg.DeviceDefaultMemory, MaxCPU: cfg.DeviceMaxCPU, MaxMemory: cfg.DeviceMaxMemory, NoCPULimit: cfg.DeviceNoCPULimit, MicroVMOverheadCPU: profilePolicyOverheadCPU, MicroVMOverheadMemory: profilePolicyOverheadMemory},
		LabNodeSelector:           labNodeSelector,
		LabTolerations:            labTolerations,
		Reader:                    mgr.GetAPIReader(),
		RuntimeObservation:        cfg.RuntimeObservation,
		RequiredSnapshotAvailable: cfg.RequiredSnapshotAvailable && cfg.State.Enabled,
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		Recorder:                  mgr.GetEventRecorderFor("lab"),
		BaseDomain:                cfg.BaseDomain,
		ProxySourceCIDRs:          cfg.ProxySourceCIDRs,
		VPNBaseNetwork:            cfg.VPNBaseNetwork,
		InetBaseNetwork:           cfg.InetBaseNetwork,
		State:                     statePolicy,
		Mirror:                    mirror,
		Resolver:                  resolver,
		NetConfigImage:            cfg.NetConfigImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Lab")
		os.Exit(1)
	}
	var scheduler *laboratorycontroller.Scheduler
	if cfg.SchedulerEnabled {
		// the scheduler asks discovery and the CRD itself before it deletes the work of a gone reservation (a deleted or
		// reinstalled CRD is no such thing)
		reservationsDC, err := discovery.NewDiscoveryClientForConfig(restCfg)
		if err != nil {
			setupLog.Error(err, "unable to build the discovery client")
			os.Exit(1)
		}
		capacity, err := operator.ParseCapacity(cfg)
		if err != nil {
			setupLog.Error(err, "unable to read the capacity")
			os.Exit(1)
		}
		// a budget needs the CRDs that keep the gang commit's fields (helm upgrade does not apply the chart's CRDs)
		capacity = laboratorycontroller.BudgetWithCurrentCRDs(context.Background(), mgr.GetAPIReader(), mgr.GetEventRecorderFor("scheduler"), capacity)
		scheduler = &laboratorycontroller.Scheduler{
			ReservationCRD: laboratorycontroller.ReservationCRDBy(reservationsDC, mgr.GetAPIReader()),
			Mirror:         mirror,
			Client:         mgr.GetClient(),
			Reader:         mgr.GetAPIReader(),
			Recorder:       mgr.GetEventRecorderFor("scheduler"),
			Config: laboratorycontroller.SchedulerConfig{
				MaxPods:                cfg.SchedulerMaxPods,
				StartupTimeout:         cfg.SchedulerStartupTimeout,
				RestartThreshold:       cfg.SchedulerRestartThreshold,
				PlatformReservePercent: cfg.SchedulerPlatformReservePercent,
				PlatformReserveNode: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cfg.SchedulerPlatformReserveCPU),
					corev1.ResourceMemory: resource.MustParse(cfg.SchedulerPlatformReserveMemory),
				},
				PackingReservePercent: cfg.SchedulerPackingReservePercent,
				PackingReserveNode: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cfg.SchedulerPackingReserveCPU),
					corev1.ResourceMemory: resource.MustParse(cfg.SchedulerPackingReserveMemory),
				},
				ResourceCheck:  cfg.SchedulerResourceCheck,
				Prepull:        cfg.SchedulerPrepull,
				PrepullTimeout: cfg.SchedulerPrepullTimeout,
				Aging:          time.Duration(cfg.SchedulerAgingSeconds) * time.Second,
				Maximum:        capacity,
				Profiles:       profilePolicy,
				NodeTimeout:    cfg.CapacityNodeTimeout,
				// the cleanup of unreserved work gives the same notice as the end of a reservation
				UnreservedNotice: time.Duration(cfg.ReservationEndingNoticeSeconds) * time.Second,
			},
			Defaults:        laboratorycontroller.DeviceDefaults{CPU: cfg.DeviceDefaultCPU, Memory: cfg.DeviceDefaultMemory, MaxCPU: cfg.DeviceMaxCPU, MaxMemory: cfg.DeviceMaxMemory, NoCPULimit: cfg.DeviceNoCPULimit, MicroVMOverheadCPU: profilePolicyOverheadCPU, MicroVMOverheadMemory: profilePolicyOverheadMemory},
			GroupPods:       cfg.GroupPods,
			LabNodeSelector: labNodeSelector,
			LabTolerations:  labTolerations,
		}
		if err = mgr.Add(scheduler); err != nil {
			setupLog.Error(err, "unable to add the scheduler")
			os.Exit(1)
		}
	}
	microVMRuntimeClass := ""
	if profilePolicy.MicroVM {
		microVMRuntimeClass = cfg.Profiles.MicroVMRuntimeClass
	}
	deviceReconciler := &laboratorycontroller.DeviceReconciler{
		Scheduled:               cfg.SchedulerEnabled,
		Recorder:                mgr.GetEventRecorderFor("device"),
		MirrorRegistries:        mirror.Registries,
		Reader:                  mgr.GetAPIReader(),
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		LabNodeSelector:         labNodeSelector,
		LabTolerations:          labTolerations,
		PriorityClass:           cfg.DevicePriorityClass,
		SchedulerName:           cfg.LabSchedulerName,
		Profiles:                profilePolicy,
		MicroVMRuntimeClass:     microVMRuntimeClass,
		TerminationGraceSeconds: cfg.LabPodTerminationGraceSeconds,
		NetConfigImage:          cfg.NetConfigImage,
		Defaults:                laboratorycontroller.DeviceDefaults{CPU: cfg.DeviceDefaultCPU, Memory: cfg.DeviceDefaultMemory, MaxCPU: cfg.DeviceMaxCPU, MaxMemory: cfg.DeviceMaxMemory, NoCPULimit: cfg.DeviceNoCPULimit, MicroVMOverheadCPU: profilePolicyOverheadCPU, MicroVMOverheadMemory: profilePolicyOverheadMemory},
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
	if served, err := (&laboratorycontroller.ReservationReconciler{
		Client:      mgr.GetClient(),
		Reader:      mgr.GetAPIReader(),
		Notice:      time.Duration(cfg.ReservationEndingNoticeSeconds) * time.Second,
		NoScheduler: !cfg.SchedulerEnabled,
		// the scheduler runs with its default status interval
		UsageMaxAge: laboratorycontroller.ReservationUsageMaxAge(0),
	}).SetupWithManagerIfServed(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Reservation")
		os.Exit(1)
	} else if !served {
		setupLog.Info("the Reservation CRD is not installed: no reservation is admitted until the CRDs are applied and the operator restarted")
		if scheduler != nil {
			// without the controller no window would end: the scheduler admits nothing under a reservation until a restart
			scheduler.NoReservationController = true
		}
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
	if cfg.RuntimeObservation {
		if err = mgr.Add(&laboratorycontroller.RetentionSweeper{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Registry: stateRegistry, RetirementsOnly: true, Interval: 5 * time.Second}); err != nil {
			setupLog.Error(err, "unable to add lifecycle retirement sweep")
			os.Exit(1)
		}
	}
	if err = mgr.Add(&laboratorycontroller.VNISweeper{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Interval: cfg.VNISweepInterval, Quarantine: cfg.VNIQuarantine, Grace: cfg.VNISweepInterval, RuntimeObservation: cfg.RuntimeObservation, Retirer: vniRetirer}); err != nil {
		setupLog.Error(err, "unable to add the VNI sweeper")
		os.Exit(1)
	}
	if cfg.EgressMonitorEnabled {
		if err = mgr.Add(&laboratorycontroller.EgressMonitor{
			Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorderFor("egress"), Log: ctrl.Log.WithName("egress-monitor"),
			Port: cfg.EgressStatsPort, LogDir: cfg.EgressLogDir, LogRetention: cfg.EgressLogRetention,
			HostLabsThreshold: cfg.EgressSignalHostLabs, CapMinutes: cfg.EgressSignalCapMinutes,
		}); err != nil {
			setupLog.Error(err, "unable to add the egress monitor")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

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

// gatewayEgressEnv are the egress settings (and the per-lab ceilings the gateway keeps when the bandwidth protection is off) of a NEW gateway.
func gatewayEgressEnv(cfg *operator.Config) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, e := range []struct{ name, value string }{
		{"EGRESS_DNS_UPSTREAMS", cfg.EgressDNSUpstreams}, {"EGRESS_DNS_QUERIES_PER_SECOND", cfg.EgressDNSQueriesPerSecond},
		{"EGRESS_HOST_NEW_PER_SECOND", cfg.EgressHostNewPerSecond}, {"EGRESS_MAX_CONN_SECONDS", cfg.EgressMaxConnSeconds},
		{"EGRESS_TUNNEL_SUFFIXES", cfg.EgressTunnelSuffixes}, {"EGRESS_ICMP_PER_SECOND", cfg.EgressICMPPerSecond},
		{"EGRESS_STATS_PORT", fmt.Sprint(cfg.EgressStatsPort)},
	} {
		if e.value != "" {
			env = append(env, corev1.EnvVar{Name: e.name, Value: e.value})
		}
	}
	if cfg.Bandwidth.LabInternetCapMbit > 0 {
		env = append(env, corev1.EnvVar{Name: "GATEWAY_DOWNLOAD_KBPS", Value: fmt.Sprint(cfg.Bandwidth.LabInternetCapMbit * 1000)})
	}
	if cfg.Bandwidth.LabUploadKbps > 0 {
		env = append(env, corev1.EnvVar{Name: "GATEWAY_UPLOAD_KBPS", Value: fmt.Sprint(cfg.Bandwidth.LabUploadKbps)})
	}
	return env
}
