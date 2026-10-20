package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/profiles"
)

// StartingTaintKey is the startup taint of a new lab worker. Infra puts it on the node when it is created (and tells the cluster
// autoscaler it is a startup taint, so the node counts as not usable yet instead of as a real node that fits nothing); the
// node-agent removes it once the node can take lab pods.
const StartingTaintKey = "cybericebox.com/node-starting"

const labelValueTrue = "true"

// StartupTaintRemover removes StartingTaintKey from the node of this node-agent once the node can really take a lab pod: the
// node-agent-ready label is set and the extended resource cybericebox.com/tun is in the node's allocatable (the kubelet has the
// device plugin registered and its devices listed). A node without the taint is left alone, so it is safe to ship before the
// taint exists. It is a manager Runnable.
type StartupTaintRemover struct {
	Client   client.Client
	NodeName string
	// TunSlots is how many tun devices the node-agent offers; 0 means the plugin is off and the resource is not waited for.
	TunSlots int
	// Interval is how often the node is looked at while the taint is on; zero means 2 seconds.
	Interval time.Duration
}

// NeedLeaderElection: every node-agent looks after its own node.
func (r *StartupTaintRemover) NeedLeaderElection() bool { return false }

// Start blocks until the taint is gone (or was never there for a minute of looking) or ctx ends.
func (r *StartupTaintRemover) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	log := ctrl.Log.WithName("startup-taint")
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		done, err := r.Reconcile(ctx)
		if err != nil {
			log.Error(err, "remove the startup taint")
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Reconcile does one step: true when there is nothing left to do (the taint is not on the node, or was just removed).
func (r *StartupTaintRemover) Reconcile(ctx context.Context) (bool, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		var node corev1.Node
		if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, &node); err != nil {
			return false, err
		}
		index := -1
		for i, taint := range node.Spec.Taints {
			if taint.Key == StartingTaintKey {
				index = i
			}
		}
		if index < 0 {
			return true, nil
		}
		if !r.ready(&node) {
			return false, nil
		}
		patch, err := removeTaintPatch(node.Spec.Taints)
		if err != nil {
			return false, err
		}
		err = r.Client.Patch(ctx, &node, client.RawPatch(types.JSONPatchType, patch))
		if err == nil {
			return true, nil
		}
		// The taints changed under us (the test operation fails) or the object did: look again.
		last = err
		if !errors.IsConflict(err) && !errors.IsInvalid(err) {
			return false, err
		}
	}
	return false, fmt.Errorf("the node kept changing: %w", last)
}

// ready is the three conditions: the node-agent is up (it set its label, which comes last), and the tun resource is allocatable.
func (r *StartupTaintRemover) ready(node *corev1.Node) bool {
	if node.Labels[names.LabelNodeAgentReady] != labelValueTrue {
		return false
	}
	if r.TunSlots <= 0 {
		return true
	}
	tun := node.Status.Allocatable[corev1.ResourceName(profiles.TUNResource)]
	return tun.Value() > 0
}

// removeTaintPatch is a JSON patch that replaces the taints with the same list without StartingTaintKey, guarded by a test of the
// list it was computed from: it applies only if the taints are still exactly those.
func removeTaintPatch(taints []corev1.Taint) ([]byte, error) {
	var kept []corev1.Taint
	for _, taint := range taints {
		if taint.Key != StartingTaintKey {
			kept = append(kept, taint)
		}
	}
	ops := []map[string]any{{"op": "test", "path": "/spec/taints", "value": taints}}
	if len(kept) == 0 {
		ops = append(ops, map[string]any{"op": "remove", "path": "/spec/taints"})
	} else {
		ops = append(ops, map[string]any{"op": "replace", "path": "/spec/taints", "value": kept})
	}
	return json.Marshal(ops)
}
