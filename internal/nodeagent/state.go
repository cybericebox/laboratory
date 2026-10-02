//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
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
		return devicestate.Forward(ctx, fmt.Sprintf("127.0.0.1:%d", cfg.StateForwardPort), cfg.StateRegistryAddr, log)
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
	engine := &devicestate.Engine{
		Runtime: rt,
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
