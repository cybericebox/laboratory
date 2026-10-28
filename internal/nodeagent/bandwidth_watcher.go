package nodeagent

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

// GeneveLimiter is what the watcher drives: the flow manager.
type GeneveLimiter interface {
	SetGeneveLimits(bandwidth.Limits) error
}

// BandwidthWatcher gives the flow manager the Geneve ceilings of this node. The operator works them out (it knows the node's label, the
// reserves of the platform pods and the labs) and writes them, with the limits of the other components, under the name of each node in
// the ConfigMap laboratory-bandwidth; the DaemonSet mounts it, and this reads the key of its own node. Nothing here watches the API,
// computes a bandwidth or polls: the kubelet refreshes the file when the operator changes it, and the flow manager touches an OVS meter
// only when a rate changes. A node-agent made without the volume has no limits.
type BandwidthWatcher struct {
	NodeName string
	// Dir is where the ConfigMap is mounted (bandwidth.ConfigMapDir).
	Dir   string
	Flows GeneveLimiter
}

// NeedLeaderElection: every node-agent watches its own node's key.
func (w *BandwidthWatcher) NeedLeaderElection() bool { return false }

// Start applies the limits at the start and at each change, until ctx ends.
func (w *BandwidthWatcher) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("bandwidth")
	dir := w.Dir
	if dir == "" {
		dir = bandwidth.ConfigMapDir
	}
	return bandwidth.WatchNode(ctx, dir, w.NodeName,
		func(l bandwidth.Limits) {
			if err := w.Flows.SetGeneveLimits(l); err != nil {
				log.Error(err, "apply the Geneve limits", "geneveKbps", l.GeneveKbps, "labCapKbps", l.LabCapKbps)
				return
			}
			log.Info("Geneve limits", "geneveKbps", l.GeneveKbps, "labCapKbps", l.LabCapKbps)
		},
		func(err error) { log.Error(err, "read the bandwidth limits of the node") })
}
