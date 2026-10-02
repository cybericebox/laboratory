//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// SetupDeviceState adds the platform registry's node side to the manager: the
// localhost registry forwarder (snapshots and the image cache are both pulled
// through it) and, with state persistence on, the snapshot engine that follows
// the snapshot-backed device containers of this node. It does nothing when no
// registry is configured.
func SetupDeviceState(mgr ctrl.Manager, cfg *Config) error {
	if cfg.StateRegistryAddr == "" {
		return nil
	}
	log := ctrl.Log.WithName("device-state")
	if err := os.MkdirAll(cfg.StateWorkDir, 0o700); err != nil {
		return fmt.Errorf("state work dir: %w", err)
	}
	local := fmt.Sprintf("localhost:%d", cfg.StateForwardPort)

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		log.Info("registry forwarder", "listen", "127.0.0.1", "port", cfg.StateForwardPort, "target", cfg.StateRegistryAddr)
		var opts []devicestate.ForwardOption
		if cfg.StateRegistryReaderUser != "" {
			opts = append(opts, devicestate.WithReader(cfg.StateRegistryReaderUser, cfg.StateRegistryReaderPassword),
				devicestate.WithHost(fmt.Sprintf("localhost:%d", cfg.StateForwardPort)))
		}
		return devicestate.Forward(ctx, fmt.Sprintf("127.0.0.1:%d", cfg.StateForwardPort), cfg.StateRegistryAddr, log, opts...)
	})); err != nil {
		return err
	}

	if !cfg.StatePersistence {
		return nil // the forwarder alone: the registry serves the image cache
	}
	rt, err := devicestate.NewContainerdRuntime(cfg.CRISock, cfg.ContainerdNamespace, cfg.CgroupRoot, log)
	if err != nil {
		return err
	}
	reg := &snapshot.Registry{Host: local}
	if cfg.StateRegistryUser != "" {
		reg.Auth = &authn.Basic{Username: cfg.StateRegistryUser, Password: cfg.StateRegistryPassword}
	}
	capacity := &snapshot.Capacity{Registry: reg, ReserveFraction: cfg.StateRegistryReserve, TTL: time.Minute}
	if cfg.StateRegistryCapacity != "" {
		q, err := resource.ParseQuantity(cfg.StateRegistryCapacity)
		if err != nil || q.Sign() < 0 {
			return fmt.Errorf("STATE_REGISTRY_CAPACITY %q: not a quantity", cfg.StateRegistryCapacity)
		}
		capacity.Total = q.Value()
	}
	if cfg.StateRegistryReserve < 0 || cfg.StateRegistryReserve >= 1 {
		return fmt.Errorf("STATE_REGISTRY_RESERVE %v must be at least 0 and below 1", cfg.StateRegistryReserve)
	}
	var budget int64
	if cfg.StatePushBudget != "" {
		q, err := resource.ParseQuantity(cfg.StatePushBudget)
		if err != nil || q.Sign() < 0 {
			return fmt.Errorf("STATE_PUSH_BUDGET %q: not a quantity", cfg.StatePushBudget)
		}
		budget = q.Value()
	}
	engine := &devicestate.Engine{
		MinPushInterval:  cfg.StateMinPushInterval,
		PushBudget:       budget,
		PushBudgetWindow: cfg.StatePushBudgetWindow,
		SupersededGrace:  cfg.StateSupersededGrace,
		Space:            capacity,
		Retained:         reg,
		Runtime:          rt,
		Cluster: &devicestate.KubeCluster{
			Client:   mgr.GetClient(),
			Reader:   mgr.GetAPIReader(),
			NodeName: cfg.NodeName,
		},
		Pusher:       reg,
		RegistryHost: local,
		WorkDir:      cfg.StateWorkDir,
		MaxWatchDirs: cfg.StateMaxWatchDirs,
		Log:          log,
	}
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		// A crash in the middle of a snapshot leaves the container frozen.
		for _, dir := range devicestate.ThawOrphans(cfg.CgroupRoot) {
			log.Info("thawed a container left frozen", "cgroup", dir)
		}
		defer rt.Close()
		return engine.Run(ctx)
	}))
}
