package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/cybericebox/laboratory/internal/agent/config"
	grpcserver "github.com/cybericebox/laboratory/internal/agent/grpc"
	"github.com/cybericebox/laboratory/internal/crdcheck"
	"github.com/cybericebox/laboratory/internal/errorlog"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/internal/profiles"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func Run() {
	// The agent logs with the standard logger: its error lines are counted into its own journal.
	journal := errorlog.New("agent", errorlog.Instance())
	log.SetOutput(errorlog.NewLineWriter(journal, os.Stderr))
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	// Batch calls write thousands of objects; the default client-side rate limit
	// (5 QPS, burst 10) would make them crawl.
	restCfg.QPS = 100
	restCfg.Burst = 200
	cs, err := grpcserver.NewVersionedClientset(restCfg)
	if err != nil {
		log.Fatalf("versioned client: %v", err)
	}
	k8s, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatalf("core client: %v", err)
	}
	// Optional: live resource-usage reporting needs metrics-server. Building the
	// client never contacts the API, so a failure here only means a malformed
	// rest config; degrade to nil (usage reported as zero) rather than crash.
	metrics, err := metricsclient.NewForConfig(restCfg)
	if err != nil {
		log.Printf("metrics client unavailable, live usage disabled: %v", err)
		metrics = nil
	}
	// Enroll must never run against a CRD that would drop the enrollment epoch: refuse to start instead.
	if err := crdcheck.Wait(context.Background(), crdcheck.FromDiscovery(k8s.Discovery()), 60*time.Second, func(err error) { log.Printf("%v", err) }); err != nil {
		log.Fatalf("%v", err)
	}
	h := grpcserver.NewHandler(cs, k8s, metrics, cfg.AgentID)
	h.SetStatePersistence(cfg.StatePersistence)
	h.EnableReadCache()
	h.SetAdmissionStaleAfter(cfg.AdmissionStaleAfter)
	h.SetRequiredSnapshotAvailable(cfg.RequiredSnapshotAvailable && cfg.StatePersistence)
	feat, err := features(cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	feat.Lifecycle = &protobuf.LifecycleFeature{}
	feat.Lifecycle.PerLabStop = cfg.PerLabStopAvailable
	feat.Lifecycle.RequiredSnapshot = cfg.RequiredSnapshotAdvertised
	feat.Lifecycle.ConfirmedRuntime = cfg.ConfirmedRuntimeAvailable
	feat.Lifecycle.RetainedRestart = cfg.RetainedRestartAvailable
	feat.Lifecycle.FullGroupStop = cfg.FullGroupStopAvailable
	h.SetFeatures(feat)
	h.SetErrorJournal(grpcserver.ErrorJournalConfig{ReleaseNamespace: cfg.ReleaseNamespace, Self: journal})
	go h.RunErrorJournal(context.Background())
	if err := cfg.GroupPods.Validate(); err != nil {
		log.Fatalf("%v", err)
	}
	h.SetGroupPods(cfg.GroupPods)
	h.SetReservationNotice(time.Duration(cfg.ReservationEndingNoticeSeconds) * time.Second)
	h.SetRenewMinInterval(cfg.Server.RenewMinInterval)
	h.SetClientCA(cfg.MTLS.ClientCAFile, cfg.MTLS.ClientCAKeyFile, cfg.MTLS.ClientCertTTL)
	h.SetRegistryAddr(cfg.RegistryAddr)
	h.SetRegistryAuth(cfg.RegistryUser, cfg.RegistryPassword)
	h.SetImagePolicy(cfg.ImageDeny, cfg.Cache.NodePrefix, cfg.RegistryAddr, cfg.RegistryAddr)
	var labSelector map[string]string
	var labTolerations []corev1.Toleration
	if err := json.Unmarshal([]byte(cfg.LabNodeSelector), &labSelector); err != nil {
		log.Fatalf("LAB_NODE_SELECTOR: %v", err)
	}
	if err := json.Unmarshal([]byte(cfg.LabTolerations), &labTolerations); err != nil {
		log.Fatalf("LAB_TOLERATIONS: %v", err)
	}
	h.SetLabScheduling(labSelector, labTolerations)
	reserve, err := nodeReserve(cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	h.SetNodeReserve(reserve)
	packing, err := packingReserve(cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	h.SetPackingReserve(packing)
	maximum, err := nodecap.ParseMaximum(cfg.CapacityBudgetCPU, cfg.CapacityBudgetMemory)
	if err != nil {
		log.Fatalf("%v", err)
	}
	// the operator runs without a budget its CRDs cannot carry: the agent then advertises none either
	maximum, why, kept := crdcheck.UsableBudget(context.Background(), crdsFrom(k8s), maximum, crdcheck.BudgetRetry)
	if kept != nil {
		log.Printf("warning: CRD check failed, budget kept: %v", kept)
	}
	if why != "" {
		log.Printf("capacity budget not advertised: %s; apply the chart CRDs (kubectl apply --server-side -f charts/laboratory/crds/) and restart", why)
	}
	h.SetMaximum(maximum)
	go h.RunTenantStatus(context.Background(), cfg.TenantStatusInterval)
	if cfg.Cache.Enabled {
		pw, err := prewarmConfig(cfg, k8s)
		if err != nil {
			log.Fatalf("cache prewarm: %v", err)
		}
		h.SetPrewarm(pw)
	}
	h.SetMonitoringConfig(grpcserver.MonitoringConfig{
		JournalSize:         cfg.Monitoring.JournalSize,
		JournalAge:          cfg.Monitoring.JournalAge,
		PollInterval:        cfg.Monitoring.PollInterval,
		SubscriberBuffer:    cfg.Monitoring.SubscriberBuffer,
		MaxStreamsPerTenant: cfg.Monitoring.MaxStreamsPerTenant,
	})
	srv, err := grpcserver.New(cfg, h)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}
	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// SIGTERM (a rollout, a drain): stop taking new calls and let the running ones finish, then close the rest. Without this a call that
	// has generated a WireGuard key but not yet answered is cut and the participant is left without its configuration.
	sig, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	finished := make(chan struct{})
	go func() {
		<-sig.Done()
		defer close(finished)
		log.Printf("shutting down: finishing running calls for up to %s", cfg.Server.ShutdownGrace)
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(cfg.Server.ShutdownGrace):
			log.Printf("shutdown grace over: closing the remaining calls")
			srv.Stop()
		}
	}()
	log.Printf("agent listening on port %s (mtls=%t)", cfg.GRPCPort, cfg.MTLSEnabled())
	if err := srv.Serve(lis); err != nil {
		log.Printf("serve: %v", err)
		os.Exit(1)
	}
	if sig.Err() != nil {
		<-finished // Serve returns when the listener closes, the running calls are still being finished
	}
}

// features is what GetFeatures reports from the agent's own configuration.
func features(cfg *config.Config) (f grpcserver.Features, err error) {
	f = grpcserver.Features{
		StatePersistence: cfg.StatePersistence, Debounce: cfg.State.Debounce, ExcludePaths: cfg.State.ExcludePaths,
		CacheEnabled: cfg.Cache.Enabled, CacheRegistries: cfg.Cache.Registries,
		SchedulerEnabled: cfg.Scheduler.Enabled, SchedulerMaxPods: int32(cfg.Scheduler.MaxPods),
		NodeTimeoutSeconds: int32(cfg.Scheduler.NodeTimeout / time.Second),
		LabsDomain:         cfg.BaseDomain, VPNEndpoint: cfg.PublicVPNEndpoint,
		ProxyAccessTokenMaxTTL: cfg.ProxyAccessTokenMaxTTL, ProxySessionIdleTTL: cfg.ProxySessionIdleTTL, ProxySessionMaxTTL: cfg.ProxySessionMaxTTL,
	}
	if f.DeviceProfiles, err = profiles.Enabled(cfg.DeviceProfiles); err != nil {
		return f, fmt.Errorf("AGENT_DEVICE_PROFILES: %w", err)
	}
	if len(f.DeviceProfiles) == 0 {
		return f, fmt.Errorf("AGENT_DEVICE_PROFILES must list at least one profile")
	}
	if f.Limits, err = cfg.Limits.Parse(); err != nil {
		return f, err
	}
	if f.Profiles, err = cfg.Profiles.Policy(); err != nil {
		return f, err
	}
	f.DeviceDefaults = limits.DeviceDefaults{CPU: cfg.Limits.DeviceDefaultCPU, Memory: cfg.Limits.DeviceDefaultMemory, MaxCPU: cfg.Limits.DeviceMaxCPU, MaxMemory: cfg.Limits.DeviceMaxMemory}
	if f.Profiles.MicroVM {
		f.DeviceDefaults.MicroVMOverheadCPU, f.DeviceDefaults.MicroVMOverheadMemory = cfg.Profiles.MicroVMOverheadCPU, cfg.Profiles.MicroVMOverheadMemory
	}
	if f.GroupPods.Sizing, err = cfg.GroupSizing.Parse(); err != nil {
		return f, err
	}
	if err = cfg.GroupLimits.Validate(); err != nil {
		return f, err
	}
	f.GroupLimits = cfg.GroupLimits
	// the pods are sized for at most this many users and labs: the maximum the agent reports is the size at these counts, so the default
	// size must fit inside THAT maximum (not the one of vpn.sizing.maxUsers, which groupLimits.maxUsers replaces)
	f.GroupPods.Sizing.VPN.MaxUnits, f.GroupPods.Sizing.Gateway.MaxUnits = cfg.GroupLimits.MaxUsers, cfg.GroupLimits.MaxLabs
	if err = cfg.GroupPods.CheckWithin(f.GroupPods.Sizing); err != nil {
		return f, err
	}
	f.GroupPods.DefaultVPN, f.GroupPods.DefaultGateway = cfg.GroupPods.VPNOverhead(), cfg.GroupPods.GatewayOverhead()
	f.MaxEntries = cfg.State.MaxEntries
	for _, c := range []struct {
		env, val string
		dst      *int64
	}{{"STATE_WRITE_QUOTA", cfg.State.WriteQuota, &f.WriteQuota}, {"STATE_MAX_FILE_SIZE", cfg.State.MaxFileSize, &f.MaxFileSize},
		{"STATE_TENANT_QUOTA", cfg.State.TenantQuota, &f.TenantQuota}} {
		q, err := resource.ParseQuantity(c.val)
		// the tenant quota may be 0: no limit
		if err != nil || q.Sign() < 0 || (q.Sign() == 0 && c.dst != &f.TenantQuota) {
			return f, fmt.Errorf("%s %q is not a valid quantity", c.env, c.val)
		}
		*c.dst = q.Value()
	}
	return f, nil
}

// nodeReserve is the scheduler's platform reserve as the operator reads it.
func nodeReserve(cfg *config.Config) (nodecap.Reserve, error) {
	if cfg.PlatformReservePercent < 0 || cfg.PlatformReservePercent >= 100 {
		return nodecap.Reserve{}, fmt.Errorf("SCHEDULER_PLATFORM_RESERVE_PERCENT must be in [0,100)")
	}
	r := nodecap.Reserve{Percent: cfg.PlatformReservePercent}
	for _, c := range []struct {
		env, val string
		cpu      bool
	}{{"SCHEDULER_PLATFORM_RESERVE_CPU", cfg.PlatformReserveCPU, true}, {"SCHEDULER_PLATFORM_RESERVE_MEMORY", cfg.PlatformReserveMemory, false}} {
		q, err := resource.ParseQuantity(c.val)
		if err != nil || q.Sign() < 0 {
			return r, fmt.Errorf("%s: %q is not a non-negative quantity", c.env, c.val)
		}
		if c.cpu {
			r.Node.CPU = q.MilliValue()
		} else {
			r.Node.Memory = q.Value()
		}
	}
	return r, nil
}

// crdsFrom reads a CustomResourceDefinition with the agent's client (the agent may get the laboratory's Device and LabGroup CRDs).
func crdsFrom(k8s kubernetes.Interface) crdcheck.GetCRD {
	return func(ctx context.Context, name string) (map[string]any, error) {
		raw, err := k8s.Discovery().RESTClient().Get().AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions", name).DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		var crd map[string]any
		if err := json.Unmarshal(raw, &crd); err != nil {
			return nil, err
		}
		return crd, nil
	}
}

// packingReserve is the scheduler's hidden packing reserve as the operator reads it.
func packingReserve(cfg *config.Config) (nodecap.PackingReserve, error) {
	if cfg.PackingReservePercent < 0 || cfg.PackingReservePercent >= 50 {
		return nodecap.PackingReserve{}, fmt.Errorf("SCHEDULER_PACKING_RESERVE_PERCENT must be in [0,50)")
	}
	r := nodecap.PackingReserve{Percent: cfg.PackingReservePercent}
	for _, c := range []struct {
		env, val string
		cpu      bool
	}{{"SCHEDULER_PACKING_RESERVE_CPU", cfg.PackingReserveCPU, true}, {"SCHEDULER_PACKING_RESERVE_MEMORY", cfg.PackingReserveMemory, false}} {
		q, err := resource.ParseQuantity(c.val)
		if err != nil || q.Sign() < 0 {
			return r, fmt.Errorf("%s: %q is not a non-negative quantity", c.env, c.val)
		}
		if c.cpu {
			r.CPU = q.MilliValue()
		} else {
			r.Memory = q.Value()
		}
	}
	return r, nil
}
