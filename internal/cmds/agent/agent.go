package agent

import (
	"log"
	"net"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/cybericebox/laboratory/internal/agent/config"
	grpcserver "github.com/cybericebox/laboratory/internal/agent/grpc"
)

func Run() {
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
	h := grpcserver.NewHandler(cs, k8s, metrics, cfg.AgentID)
	if cfg.Cache.Enabled {
		pw, err := prewarmConfig(cfg, k8s)
		if err != nil {
			log.Fatalf("cache prewarm: %v", err)
		}
		h.SetPrewarm(pw)
	}
	h.SetMonitoringConfig(grpcserver.MonitoringConfig{
		JournalSize:      cfg.Monitoring.JournalSize,
		JournalAge:       cfg.Monitoring.JournalAge,
		PollInterval:     cfg.Monitoring.PollInterval,
		SubscriberBuffer: cfg.Monitoring.SubscriberBuffer,
	})
	srv, err := grpcserver.New(cfg, h)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}
	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("agent listening on port %s (mtls=%t)", cfg.GRPCPort, cfg.MTLS.Enabled)
	if err := srv.Serve(lis); err != nil {
		log.Printf("serve: %v", err)
		os.Exit(1)
	}
}
