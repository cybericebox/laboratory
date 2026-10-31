package nodeagent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
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

	// mu guards Annotations: the retry of the start-up check changes a value (SetAnnotation) while Start asserts the others again.
	mu sync.Mutex
}

// SetAnnotation changes one of the annotations the labeler keeps asserting. Whoever learns a newer result of a check puts it here (and
// on the node); a value patched on the node alone would be put back to the start-up result at the next assertion.
func (l *NodeLabeler) SetAnnotation(key, value string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Annotations == nil {
		l.Annotations = map[string]string{}
	}
	l.Annotations[key] = value
}

// annotations is a copy of the annotations to assert now.
func (l *NodeLabeler) annotations() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.Annotations))
	for k, v := range l.Annotations {
		out[k] = v
	}
	return out
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
	var lastErr string
	for {
		if err := l.set(ctx, true); err != nil {
			// not silent: a node that is never marked ready takes no lab (an admission policy that refuses the patch, a missing right)
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				ctrl.Log.WithName("node-labeler").Error(err, "cannot mark the node ready for labs; retrying")
			}
		} else {
			lastErr = ""
		}
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
	annotations := l.annotations()
	if ready && len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	patch, _ := json.Marshal(map[string]any{"metadata": meta})
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: l.NodeName}}
	if err := l.Client.Patch(ctx, node, client.RawPatch(client.Merge.Type(), patch)); err != nil {
		return err
	}
	if ready && !l.told && l.Recorder != nil {
		l.told = true
		for k, v := range annotations {
			if why, bad := IsUnavailable(v); bad {
				l.Recorder.Eventf(node, corev1.EventTypeWarning, "LifecyclePreflightFailed", "%s: %s", k, why)
			}
		}
	}
	return nil
}
