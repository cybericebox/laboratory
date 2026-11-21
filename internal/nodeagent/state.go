//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// SetupRegistryForwarder adds the platform registry's node side to the manager: the localhost registry forwarder (snapshots and the
// image cache are both pulled through it, and the node-state DaemonSet pushes the snapshots through it: it has no network path to the
// registry of its own). The snapshot engine itself is the node-state DaemonSet's (SetupStateEngine). It does nothing when no registry is
// configured.
func SetupRegistryForwarder(mgr ctrl.Manager, cfg *Config) error {
	if cfg.StateRegistryAddr == "" {
		return nil
	}
	log := ctrl.Log.WithName("registry-forwarder")
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		log.Info("registry forwarder", "listen", "127.0.0.1", "port", cfg.StateForwardPort, "target", cfg.StateRegistryAddr)
		opts := []devicestate.ForwardOption{devicestate.WithNodeToken(cfg.RegistryTokenFile)}
		if cfg.RegistryInsecureDev {
			opts = []devicestate.ForwardOption{devicestate.WithPlainDevelopment()}
		}
		if cfg.StatePersistence {
			opts = append(opts, devicestate.WithPushSocket(cfg.StatePushSocket, cfg.StatePushSocketUID))
		}
		if cfg.StateRegistryCAFile != "" {
			opts = append(opts, devicestate.WithTLS(cfg.StateRegistryCAFile))
		}
		return devicestate.Forward(ctx, fmt.Sprintf("127.0.0.1:%d", cfg.StateForwardPort), cfg.StateRegistryAddr, log, opts...)
	}))
}

// SetupStateEngine adds the snapshot engine that follows the snapshot-backed device containers of this node. The node-state DaemonSet
// calls it when persistence is on and the start-up check passes (PreflightRetry calls it later when the check passes after it failed).
func SetupStateEngine(mgr ctrl.Manager, cfg *Config) error {
	log := ctrl.Log.WithName("device-state")
	if err := os.MkdirAll(cfg.StateWorkDir, 0o700); err != nil {
		return fmt.Errorf("state work dir: %w", err)
	}
	local := fmt.Sprintf("localhost:%d", cfg.StateForwardPort)
	rt, err := devicestate.NewContainerdRuntime(cfg.CRISock, cfg.ContainerdNamespace, cfg.CgroupRoot, log)
	if err != nil {
		return err
	}
	// A snapshot is pushed through the unix socket of the relay (the TCP port refuses every write); the client keeps the logical host.
	reg := &snapshot.Registry{Host: local, Transport: devicestate.UnixTransport(cfg.StatePushSocket)}
	var tempBudget int64
	if cfg.StateTempBudget != "" {
		q, err := resource.ParseQuantity(cfg.StateTempBudget)
		if err != nil || q.Sign() < 0 {
			return fmt.Errorf("STATE_TEMP_BUDGET %q: not a quantity", cfg.StateTempBudget)
		}
		tempBudget = q.Value()
	}
	engine := &devicestate.Engine{
		NodeAgentEpoch: uuid.NewString(),
		Runtime:        rt,
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

		MaxConcurrentDiffs: cfg.StateMaxConcurrentDiffs,
		TempBudgetBytes:    tempBudget,
	}
	if label := devicestate.AppArmorLabel(); devicestate.AppArmorBlocksSnapshots(label) {
		// The runtime's default profile has `deny mount` and the snapshot filter mounts read-only overlays: every snapshot would
		// fail with "permission denied". Said once here and on every device, instead of a failure per snapshot.
		engine.SnapshotsBlocked = fmt.Sprintf("AppArmor blocks snapshots: node-state runs under the AppArmor profile %q, "+
			"which denies mount, so device state is not saved (the node-state container needs appArmorProfile Unconfined, see DEPLOY.md)", label)
		log.Error(devicestate.ErrSnapshotsBlocked, "state persistence is on but snapshots cannot be taken on this node",
			"apparmor", label, "fix", "appArmorProfile Unconfined on the node-state container (chart: devices.statePersistence.enabled)")
	}
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		owner, err := devicestate.AcquireCaptureOwner(cfg.StateWorkDir)
		if err != nil {
			return err
		}
		defer func() { _ = owner.Close() }()
		// Invalidate old required captures before any live task can thaw.
		protected, err := engine.RecoverCaptureHolds(ctx)
		if err != nil {
			return fmt.Errorf("required capture recovery: %w", err)
		}
		for _, dir := range devicestate.ThawOrphansExcept(cfg.CgroupRoot, protected) {
			log.Info("thawed a container left frozen", "cgroup", dir)
		}
		defer func() { _ = rt.Close() }()
		return engine.Run(ctx)
	}))
}
