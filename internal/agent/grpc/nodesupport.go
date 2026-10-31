package grpc

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// What the cluster really supports. The chart says which lifecycle features it is configured for; the node-agents check their nodes at
// start (persistence: the containerd root, the container runtime, the snapshot registry; observation: the cgroup tree and the container
// runtime) and publish the result on the Node (names.AnnotationStatePersistence / AnnotationRuntimeObservation). GetFeatures reports a
// feature as available only when every lab node that serves pods confirms it, and says why not otherwise, so a caller knows what it can
// ask of this cluster.

// nodeVerdict is what the lab nodes say about the features the chart is configured for. An empty reason is "confirmed (or not asked)".
type nodeVerdict struct {
	checked                              int
	persistenceReason, observationReason string
}

// verifyNodes reads the annotations of the lab nodes that are ready for pods. A node that has not published a result (a node-agent of
// an older version, or one that is still starting), or says it has not the feature, holds the feature back. With no ready node there is
// nothing to ask: checked is 0 and the configured values stand.
func verifyNodes(nodes []corev1.Node, selector map[string]string, tolerations []corev1.Toleration, wantPersistence, wantObservation bool) nodeVerdict {
	var v nodeVerdict
	var persistence, observation []string
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[names.LabelNodeAgentReady] != "true" || !nodecap.Schedulable(n, selector, tolerations) {
			continue
		}
		v.checked++
		if wantPersistence {
			if why := annotationProblem(n, names.AnnotationStatePersistence); why != "" {
				persistence = append(persistence, fmt.Sprintf("node %s: %s", n.Name, why))
			}
		}
		if wantObservation {
			if why := annotationProblem(n, names.AnnotationRuntimeObservation); why != "" {
				observation = append(observation, fmt.Sprintf("node %s: %s", n.Name, why))
			}
		}
	}
	v.persistenceReason, v.observationReason = joinReasons(persistence), joinReasons(observation)
	return v
}

// annotationProblem is "" for a confirmed feature, else what the node says.
func annotationProblem(n *corev1.Node, key string) string {
	switch v, ok := n.Annotations[key]; {
	case !ok:
		return "its node-agent has not reported (not started yet, or an older version)"
	case v == nodeagent.PreflightReady:
		return ""
	case v == nodeagent.PreflightDisabled:
		return "not enabled on the node (the node-agent is configured without it)"
	default:
		if why, bad := nodeagent.IsUnavailable(v); bad {
			return why
		}
		return "unknown state " + v
	}
}

// joinReasons names the first failing node in full and counts the rest.
func joinReasons(r []string) string {
	sort.Strings(r)
	switch len(r) {
	case 0:
		return ""
	case 1:
		return r[0]
	}
	return fmt.Sprintf("%s (and %d more nodes)", r[0], len(r)-1)
}

// nodeVerdict reads the nodes for the features the chart configured.
func (h *Handler) nodeVerdict(ctx context.Context, wantPersistence, wantObservation bool) (nodeVerdict, error) {
	if !wantPersistence && !wantObservation {
		return nodeVerdict{}, nil
	}
	list, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nodeVerdict{}, err
	}
	return verifyNodes(list.Items, h.labSelector, h.labTolerations, wantPersistence, wantObservation), nil
}

// applyNodeSupport holds back, in the answer, what the nodes do not confirm.
func applyNodeSupport(base *protobuf.FeaturesResponse, v nodeVerdict) {
	if v.checked == 0 {
		return
	}
	var reasons []string
	if v.persistenceReason != "" {
		if base.StatePersistence != nil {
			base.StatePersistence.Available = false
			base.StatePersistence.UnavailableReason = "state persistence: " + v.persistenceReason
		}
		if base.Lifecycle != nil {
			base.Lifecycle.RetainedRestart = false
		}
		reasons = append(reasons, "state persistence: "+v.persistenceReason)
	}
	if v.observationReason != "" && base.Lifecycle != nil {
		l := base.Lifecycle
		l.PerLabStop, l.RequiredSnapshot, l.ConfirmedRuntime, l.RetainedRestart, l.FullGroupStop = false, false, false, false, false
		reasons = append(reasons, "runtime observation: "+v.observationReason)
	}
	if base.Lifecycle != nil && len(reasons) > 0 {
		base.Lifecycle.UnavailableReason = strings.Join(reasons, "; ")
	}
}

// lifecycleConfigured says some lifecycle capability is on in the configuration, which needs the native observation of the nodes.
func lifecycleConfigured(l *protobuf.LifecycleFeature) bool {
	return l != nil && (l.PerLabStop || l.RequiredSnapshot || l.ConfirmedRuntime || l.RetainedRestart || l.FullGroupStop)
}
