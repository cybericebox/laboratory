package nodeagent

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
)

// NodeLabeler marks the node of this node-agent as ready for lab pods with the label names.LabelNodeAgentReady, for as long as it runs,
// and removes the label when it stops. Every lab pod requires the label, so nothing is placed on a node whose node-agent is not
// there: a device pod on such a node would have no cni-gate and no OVS wiring. It is a manager Runnable, started after the
// controllers' caches, i.e. once the node-agent can serve pods (OVS is programmed, the gRPC socket listens).
type NodeLabeler struct {
	Client   client.Client
	NodeName string
	// Interval is how often the label is asserted again (someone may remove it); zero means 1 minute.
	Interval time.Duration
	// Annotations are put on the node in the same patch as the label, so a node that is ready for labs always has them: the result of
	// the start-up check (Preflight). Recorder, when set, tells a failed check as a Warning Event on the node, once.
	Annotations map[string]string
	Recorder    record.EventRecorder
	told        bool
}

// NeedLeaderElection: every node-agent labels its own node.
func (l *NodeLabeler) NeedLeaderElection() bool { return false }

// Start blocks until ctx ends, then removes the label.
func (l *NodeLabeler) Start(ctx context.Context) error {
	interval := l.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_ = l.set(ctx, true)
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return l.set(rctx, false)
		case <-t.C:
		}
	}
}

// set puts the label on the node, or removes it (a merge patch with null).
func (l *NodeLabeler) set(ctx context.Context, ready bool) error {
	var value any
	if ready {
		value = "true"
	}
	meta := map[string]any{"labels": map[string]any{names.LabelNodeAgentReady: value}}
	if ready && len(l.Annotations) > 0 {
		meta["annotations"] = l.Annotations
	}
	patch, _ := json.Marshal(map[string]any{"metadata": meta})
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: l.NodeName}}
	if err := l.Client.Patch(ctx, node, client.RawPatch(client.Merge.Type(), patch)); err != nil {
		return err
	}
	if ready && !l.told && l.Recorder != nil {
		l.told = true
		for k, v := range l.Annotations {
			if why, bad := IsUnavailable(v); bad {
				l.Recorder.Eventf(node, corev1.EventTypeWarning, "LifecyclePreflightFailed", "%s: %s", k, why)
			}
		}
	}
	return nil
}
