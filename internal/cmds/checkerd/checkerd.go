//go:build linux

// Package checkerd is the lab checker: a DaemonSet of its own (apart from the node-agent) that runs the checks of the labs whose
// device pods are on its node and writes the results into the Lab status. It accepts no inbound connection: no listener, no metrics
// endpoint, no health port; the kubelet's probes run the checker-health command against two files.
package checkerd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/checker"
	"github.com/cybericebox/laboratory/pkg/config"
)

// Config comes from the environment the chart sets.
type Config struct {
	NodeName string `env:"NODE_NAME,required"`
	PodName  string `env:"POD_NAME,required"`
	// Namespace is where the keepalive Lease is made (the release namespace).
	Namespace string `env:"POD_NAMESPACE,required"`
	Version   string `env:"CHECKER_VERSION"`
	CRISock   string `env:"CRI_SOCK" envDefault:"/run/k0s/containerd.sock"`
	// ContainerdState is the containerd state directory of the node, mounted read-only at the same path; ContainerdNamespace is the
	// namespace of the kubelet's CRI.
	ContainerdState     string `env:"CONTAINERD_STATE" envDefault:"/run/k0s/containerd"`
	ContainerdNamespace string `env:"CONTAINERD_NAMESPACE" envDefault:"k8s.io"`
	MaxConcurrent       int    `env:"CHECKER_MAX_CONCURRENT" envDefault:"4"`
	// RunDir holds the two files the probes look at.
	RunDir string `env:"CHECKER_RUN_DIR" envDefault:"/run/checker"`
}

const (
	liveFile  = "live"
	readyFile = "ready"
	// liveMaxAge is how old the live file may be: the engine touches it every second, so a stuck process fails the probe.
	liveMaxAge = 60 * time.Second
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func touch(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		if f, cerr := os.Create(path); cerr == nil {
			_ = f.Close()
		}
	}
}

// Health is the command of the kubelet's exec probes: "checker-health live" or "checker-health ready". It exits 0 when the
// checker's file is fresh (live) or present (ready).
func Health() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	dir := os.Getenv("CHECKER_RUN_DIR")
	if dir == "" {
		dir = "/run/checker"
	}
	st, err := os.Stat(filepath.Join(dir, os.Args[1]))
	switch {
	case err != nil:
		os.Exit(1)
	case os.Args[1] == liveFile && time.Since(st.ModTime()) > liveMaxAge:
		os.Exit(1)
	}
}

// Run starts the checker.
func Run() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("checker")
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		log.Error(err, "checker stopped")
		os.Exit(1)
	}
}

func run(cfg Config) error {
	log := ctrl.Log.WithName("checker")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(cfg.RunDir, readyFile))

	restCfg := ctrl.GetConfigOrDie()
	restCfg.QPS, restCfg.Burst = 20, 40
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	byObject, err := checker.CacheOptions(cfg.NodeName)
	if err != nil {
		return err
	}
	informers, err := cache.New(restCfg, cache.Options{Scheme: scheme, ByObject: byObject})
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}

	cri, err := checker.DialCRI(cfg.CRISock)
	if err != nil {
		return err
	}
	defer func() { _ = cri.Close() }()
	backend := &checker.NodeBackend{CRI: cri, StateDir: cfg.ContainerdState, Namespace: cfg.ContainerdNamespace}
	state := preflight(ctx, cfg, cri)
	log.Info("start-up check", "state", state)

	engine := checker.NewEngine(checker.Config{
		Backend: backend, Writer: &checker.StatusWriter{Client: direct}, MaxConcurrent: cfg.MaxConcurrent, Log: log,
	})
	watcher := &checker.Watcher{Engine: engine, Reader: direct, Log: log}
	go func() {
		if err := informers.Start(ctx); err != nil {
			log.Error(err, "informers stopped")
			stop()
		}
	}()
	if !informers.WaitForCacheSync(ctx) {
		return fmt.Errorf("the informer caches did not sync")
	}
	if err := watcher.Start(ctx, informers); err != nil {
		return err
	}
	touch(filepath.Join(cfg.RunDir, liveFile))
	touch(filepath.Join(cfg.RunDir, readyFile))

	keep := &checker.Keepalive{Client: direct, Namespace: cfg.Namespace, Node: cfg.NodeName, Holder: cfg.PodName, Version: cfg.Version,
		State: func() string { return state }}
	go keep.Run(ctx, func(err error) { log.Info("keepalive failed", "error", err.Error()) })

	beatAt := time.Time{}
	engine.Run(ctx, func() {
		if time.Since(beatAt) > 5*time.Second {
			beatAt = time.Now()
			touch(filepath.Join(cfg.RunDir, liveFile))
		}
	})
	return nil
}

// preflight tells whether the node can read the files of its containers: "ready", or "unavailable: <CODE>: <detail>" (the form the
// node-agent uses on the Node). The checker runs either way: custom checks and micro-VM devices do not need the state directory.
func preflight(_ context.Context, cfg Config, _ checker.CRI) string {
	dir := filepath.Join(cfg.ContainerdState, "io.containerd.runtime.v2.task", cfg.ContainerdNamespace)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "unavailable: STATE_DIR_MISSING: " + dir + " is not a directory; check checks.containerdState and the mount"
	}
	return "ready"
}
