//go:build linux

package nodeagent

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
)

// NewNodeLabeler is the labeler of this node with the result of the start-up check (runtime observation) as the annotations it keeps
// asserting.
func NewNodeLabeler(mgr ctrl.Manager, cfg *Config, pre PreflightResult) *NodeLabeler {
	// The state-persistence annotation is the node-state DaemonSet's (it owns the snapshot engine it describes).
	return &NodeLabeler{Client: mgr.GetClient(), NodeName: cfg.NodeName, Annotations: withoutStatePersistence(pre.Annotations()), Recorder: mgr.GetEventRecorderFor("node-agent-preflight")}
}

// SetupNodeReadiness adds the two things that make this node usable for labs, in order: the label that lab pods require, then the
// removal of the startup taint, which waits for the label and for the tun resource.
func SetupNodeReadiness(mgr ctrl.Manager, cfg *Config, labeler *NodeLabeler) error {
	if err := mgr.Add(labeler); err != nil {
		return fmt.Errorf("node labeler: %w", err)
	}
	if err := mgr.Add(&StartupTaintRemover{Client: mgr.GetClient(), NodeName: cfg.NodeName, TunSlots: cfg.TunSlots}); err != nil {
		return fmt.Errorf("startup taint remover: %w", err)
	}
	return nil
}
