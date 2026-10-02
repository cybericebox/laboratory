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
	"github.com/cybericebox/laboratory/internal/profiles"
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
	feat, err := features(cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	h.SetFeatures(feat)
	h.SetErrorJournal(grpcserver.ErrorJournalConfig{ReleaseNamespace: cfg.ReleaseNamespace, Self: journal})
	go h.RunErrorJournal(context.Background())
	if err := cfg.GroupPods.Validate(); err != nil {
		log.Fatalf("%v", err)
	}
	h.SetGroupOverhead(cfg.GroupPods.Overhead())
	h.SetRenewMinInterval(cfg.Server.RenewMinInterval)
	h.SetClientCA(cfg.MTLS.ClientCAFile, cfg.MTLS.ClientCAKeyFile, cfg.MTLS.ClientCertTTL)
	h.SetRegistryAddr(cfg.RegistryAddr)
	h.SetRegistryAuth(cfg.RegistryUser, cfg.RegistryPassword)
	h.SetImagePolicy(cfg.ImageDeny, cfg.Cache.NodePrefix, cfg.Cache.RegistryAddr, cfg.RegistryAddr)
	var labSelector map[string]string
	var labTolerations []corev1.Toleration
	if err := json.Unmarshal([]byte(cfg.LabNodeSelector), &labSelector); err != nil {
		log.Fatalf("AGENT_LAB_NODE_SELECTOR: %v", err)
	}
	if err := json.Unmarshal([]byte(cfg.LabTolerations), &labTolerations); err != nil {
		log.Fatalf("AGENT_LAB_TOLERATIONS: %v", err)
	}
	h.SetLabScheduling(labSelector, labTolerations)
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
	log.Printf("agent listening on port %s (mtls=%t)", cfg.GRPCPort, cfg.MTLS.Enabled)
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
		LabsDomain: cfg.BaseDomain, VPNEndpoint: cfg.PublicVPNEndpoint,
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
	f.MaxEntries = cfg.State.MaxEntries
	for _, c := range []struct {
		env, val string
		dst      *int64
	}{{"AGENT_STATE_WRITE_QUOTA", cfg.State.WriteQuota, &f.WriteQuota}, {"AGENT_STATE_MAX_FILE_SIZE", cfg.State.MaxFileSize, &f.MaxFileSize},
		{"AGENT_STATE_TENANT_QUOTA", cfg.State.TenantQuota, &f.TenantQuota}} {
		q, err := resource.ParseQuantity(c.val)
		// the tenant quota may be 0: no limit
		if err != nil || q.Sign() < 0 || (q.Sign() == 0 && c.dst != &f.TenantQuota) {
			return f, fmt.Errorf("%s %q is not a valid quantity", c.env, c.val)
		}
		*c.dst = q.Value()
	}
	return f, nil
}
