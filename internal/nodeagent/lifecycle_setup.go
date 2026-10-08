//go:build linux

package nodeagent

import (
	"fmt"
	containerd "github.com/containerd/containerd/v2/client"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"strings"
)

// SetupRuntimeObservation only starts an explicitly enabled native producer. It
// never turns the advertised support flags on, and bounds each scan deadline.
func SetupRuntimeObservation(mgr ctrl.Manager, cfg *Config, network *NetworkAttachReconciler) error {
	if !cfg.RuntimeObservation {
		return nil
	}
	if cfg.RuntimeObservationInterval < 1e9 || cfg.RuntimeObservationInterval > 60e9 {
		return fmt.Errorf("runtime observation interval must be 1s..1m")
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return e
	}
	journalDir := filepath.Join(cfg.StateWorkDir, "runtime-observations")
	if e := os.MkdirAll(journalDir, 0700); e != nil {
		return e
	}
	owner, e := os.OpenFile(filepath.Join(journalDir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	if e = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		owner.Close()
		return fmt.Errorf("native runtime journal already owned: %w", e)
	}
	rt, e := containerd.New(cfg.CRISock, containerd.WithDefaultNamespace(cfg.ContainerdNamespace))
	if e != nil {
		owner.Close()
		return e
	}
	o := &NativeRuntimeObserver{Runtime: rt, Namespace: cfg.ContainerdNamespace, NodeName: cfg.NodeName, BootID: strings.TrimSpace(string(boot)), CgroupRoot: cfg.CgroupRoot, JournalDir: filepath.Join(cfg.StateWorkDir, "runtime-observations"), Network: network}
	network.OVS.RuntimeRetirement = o.preparePortRetirement
	return mgr.Add(&LifecycleReporter{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Observer: o, Interval: cfg.RuntimeObservationInterval, Owner: owner})
}
