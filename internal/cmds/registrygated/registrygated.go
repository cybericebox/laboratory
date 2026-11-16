//go:build linux

// Package registrygated is the registry gate: a container of the registry pod, in front of zot. Every request to the registry
// (the relay of the nodes, the operator, the agent) reaches this process, and only this process reaches zot, which listens on the
// loopback of the pod. docs/research/2026-11-16-zot-tenant-cache.md.
package registrygated

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cybericebox/laboratory/internal/registrygate"
	"github.com/cybericebox/laboratory/pkg/config"
)

// Config comes from the environment the chart sets.
type Config struct {
	// Listen is the registry port of the pod (the Service's target); HealthListen the port of the probes.
	Listen       string `env:"GATE_LISTEN" envDefault:":5000"`
	HealthListen string `env:"GATE_HEALTH_LISTEN" envDefault:":5002"`
	// Zot is where zot listens: the loopback of the pod.
	Zot string `env:"GATE_ZOT_ADDR" envDefault:"127.0.0.1:5001"`
}

// Run is the command "/node registry-gate".
func Run() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("registry-gate")
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}
	gate, err := registrygate.New(cfg.Zot)
	if err != nil {
		log.Error(err, "gate")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	servers := []*http.Server{
		registrygate.NewServer(cfg.Listen, gate),
		registrygate.NewServer(cfg.HealthListen, gate.HealthHandler()),
	}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errs <- s.ListenAndServe() }()
	}
	log.Info("registry gate", "listen", cfg.Listen, "health", cfg.HealthListen, "zot", cfg.Zot)
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "serve")
			os.Exit(1)
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
}
