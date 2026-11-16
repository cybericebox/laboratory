//go:build linux

// Package nodestated is the node-state DaemonSet: the part of the node's work that needs the container runtime's files and the host's
// cgroups and is not pod networking. It runs the snapshot engine of device state (only with devices.statePersistence.enabled) and the
// CPU throttle monitor, and keeps the Node annotation of its own check (cybericebox.com/state-persistence). The node-agent, which wires
// every pod and cannot be allowed to stop, keeps none of those privileges (no AppArmor Unconfined, no DAC_READ_SEARCH, no containerd root).
//
// It reaches the platform registry only through the relay the node-agent serves on the loopback of the node (127.0.0.1:5035): it has no
// network path to the registry of its own. The runtime observer stays in the node-agent (it is part of the OVS retirement there).
package nodestated

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/errorlog"
	"github.com/cybericebox/laboratory/internal/health"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent"
	_ "github.com/cybericebox/laboratory/pkg/runtime"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

// relayAddr is the registry as this process sees it: the node-agent's relay on the loopback of the node.
func relayAddr(cfg *nodeagent.Config) string {
	return fmt.Sprintf("127.0.0.1:%d", cfg.StateForwardPort)
}

// Run is the command "/node node-state".
func Run() {
	journalLog, journal := errorlog.Setup("node-state", errorlog.Instance(), zap.New())
	ctrl.SetLogger(journalLog)
	log := ctrl.Log.WithName("node-state")

	cfg, err := nodeagent.LoadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		// No metrics endpoint: it is on the host network, so it would be open on the node's address.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: cfg.HealthAddr,
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	// The snapshot engine lists the pods of its node by this index.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
		return []string{obj.(*corev1.Pod).Spec.NodeName}
	}); err != nil {
		log.Error(err, "index pods by node")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		log.Error(err, "add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("caches", health.CacheSynced(mgr.GetCache())); err != nil {
		log.Error(err, "add health check")
		os.Exit(1)
	}

	if err := Setup(mgr, cfg, log); err != nil {
		log.Error(err, "set up node-state")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	log.Info("starting node-state", "node", cfg.NodeName, "statePersistence", cfg.StatePersistence)
	errorlog.Start(ctx, journal, restCfg, errorlog.Namespace(names.SystemNamespace), ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "manager error: %v\n", err)
		os.Exit(1)
	}
}

// Setup adds what node-state runs to the manager: the CPU throttle monitor, always; with state persistence asked for, the check of it
// (published on the Node), its retry while it fails and the snapshot engine once it passes.
func Setup(mgr ctrl.Manager, cfg *nodeagent.Config, log logr.Logger) error {
	if err := mgr.Add(&nodeagent.CPUThrottleMonitor{Client: mgr.GetClient(), NodeName: cfg.NodeName, CgroupRoot: cfg.CgroupRoot}); err != nil {
		return fmt.Errorf("cpu throttle monitor: %w", err)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeName}}
	rec := mgr.GetEventRecorderFor("node-state-preflight")
	annotator := &nodeagent.NodeAnnotator{Client: mgr.GetClient(), NodeName: cfg.NodeName, Recorder: rec}

	asked := cfg.StatePersistence
	input := cfg.PreflightInput()
	input.Observation = false           // the runtime observer is the node-agent's
	input.RegistryAddr = relayAddr(cfg) // no direct path to the registry: through the node-agent's relay
	pre := nodeagent.Preflight(context.Background(), input)
	for _, f := range pre.Failures() {
		log.Info("WARNING: not available on this node, switched off", "reason", f)
	}
	cfg.Apply(pre)
	annotator.Set(names.AnnotationStatePersistence, pre.StatePersistence)
	if err := mgr.Add(annotator); err != nil {
		return fmt.Errorf("node annotator: %w", err)
	}

	start, retry := nodeagent.EnginePlan(asked, pre.StatePersistence)
	if start {
		if err := nodeagent.SetupStateEngine(mgr, cfg); err != nil {
			return fmt.Errorf("snapshot engine: %w", err)
		}
	}
	if !retry {
		return nil
	}
	return mgr.Add(&nodeagent.PreflightRetry{
		Log: log.WithName("preflight-retry"), Input: input, First: pre.StatePersistence,
		Enable: func() error {
			cfg.StatePersistence = true
			if err := nodeagent.SetupStateEngine(mgr, cfg); err != nil {
				cfg.StatePersistence = false
				return err
			}
			return nil
		},
		Annotate: func(ctx context.Context, value string) error {
			return annotator.SetAnnotation(ctx, names.AnnotationStatePersistence, value)
		},
		Told: func(message string) { rec.Event(node, corev1.EventTypeNormal, "LifecyclePreflightRecovered", message) },
	})
}
