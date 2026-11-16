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
)

// NodeAnnotator keeps annotations on the node of this process, asserting them again every Interval (someone may remove one). It is
// the node-state DaemonSet's counterpart of NodeLabeler, which marks the node ready for lab pods: the annotator sets annotations only,
// never a label, and does not touch the node when it stops (the last result stays, as the node-agent's annotation of runtime
// observation does). Recorder, when set, tells a failed check as a Warning Event on the node, once per value.
type NodeAnnotator struct {
	Client   client.Client
	NodeName string
	Interval time.Duration
	Recorder record.EventRecorder

	mu          sync.Mutex
	annotations map[string]string
	told        map[string]string
}

// Set changes one annotation the annotator keeps asserting; it reaches the node at the next assertion (Start asserts at once).
func (a *NodeAnnotator) Set(key, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.annotations == nil {
		a.annotations = map[string]string{}
	}
	a.annotations[key] = value
}

// SetAnnotation is Set and puts the value on the node now (the error of that patch is returned; the value is asserted again at the
// next interval either way).
func (a *NodeAnnotator) SetAnnotation(ctx context.Context, key, value string) error {
	a.Set(key, value)
	return a.patch(ctx)
}

// NeedLeaderElection: every node-state looks after its own node.
func (a *NodeAnnotator) NeedLeaderElection() bool { return false }

// Start asserts the annotations until ctx ends.
func (a *NodeAnnotator) Start(ctx context.Context) error {
	interval := a.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastErr string
	for {
		if err := a.patch(ctx); err != nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				ctrl.Log.WithName("node-annotator").Error(err, "cannot put the check result on the node; retrying")
			}
		} else {
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (a *NodeAnnotator) patch(ctx context.Context) error {
	a.mu.Lock()
	snapshot := make(map[string]string, len(a.annotations))
	for k, v := range a.annotations {
		snapshot[k] = v
	}
	a.mu.Unlock()
	if len(snapshot) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": snapshot}})
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: a.NodeName}}
	if err := a.Client.Patch(ctx, node, client.RawPatch(client.Merge.Type(), body)); err != nil {
		return err
	}
	if a.Recorder != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.told == nil {
			a.told = map[string]string{}
		}
		for k, v := range snapshot {
			if why, bad := IsUnavailable(v); bad && a.told[k] != v {
				a.told[k] = v
				a.Recorder.Eventf(node, corev1.EventTypeWarning, "LifecyclePreflightFailed", "%s: %s", k, why)
			}
		}
	}
	return nil
}
