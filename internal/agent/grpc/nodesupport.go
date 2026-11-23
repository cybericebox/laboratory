package grpc

import (
	"context"
	"fmt"

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

// nodeVerdict is what the lab nodes say about the features the chart is configured for. A reason is a fixed code and a count, never a
// node name, a path or an error text: "CODE:n/m" with n the lab nodes that do not confirm the feature and m the lab nodes asked. An empty
// reason is "confirmed (or not asked)". The detail is on the Node (annotation, Event) for the operator of the cluster.
type nodeVerdict struct {
	checked                              int
	persistenceReason, observationReason string
	// relayAsked: console cables are switched on, so the relay of the lab nodes was asked; relayReason is "" when every ready lab node
	// (and at least one) says its relay is ready (names.AnnotationConsoleRelay).
	relayAsked  bool
	relayReason string
}

// verifyNodes reads the annotations of the lab nodes that are ready for pods. A node that has not published a result (a node-agent of
// an older version, or one that is still starting), or says it has not the feature, holds the feature back. With no ready node there is
// nothing to ask: checked is 0 and the configured values stand.
func verifyNodes(nodes []corev1.Node, selector map[string]string, tolerations []corev1.Toleration, wantPersistence, wantObservation bool) nodeVerdict {
	var v nodeVerdict
	persistence, observation := map[string]int{}, map[string]int{}
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[names.LabelNodeAgentReady] != "true" || !nodecap.Schedulable(n, selector, tolerations) {
			continue
		}
		v.checked++
		if wantPersistence {
			if code := annotationCode(n, names.AnnotationStatePersistence, nodeagent.CodePersistenceDisabled); code != "" {
				persistence[code]++
			}
		}
		if wantObservation {
			if code := annotationCode(n, names.AnnotationRuntimeObservation, nodeagent.CodeObservationDisabled); code != "" {
				observation[code]++
			}
		}
	}
	v.persistenceReason, v.observationReason = codeReason(persistence, v.checked), codeReason(observation, v.checked)
	return v
}

// annotationCode is "" for a confirmed feature, else the code of what the node says.
func annotationCode(n *corev1.Node, key, disabled string) string {
	v, ok := n.Annotations[key]
	switch {
	case !ok:
		return nodeagent.CodeNodeUnconfirmed
	case v == nodeagent.PreflightReady:
		return ""
	case v == nodeagent.PreflightDisabled:
		return disabled
	}
	if code, bad := nodeagent.CodeOf(v); bad {
		return code
	}
	return nodeagent.CodeNodeUnconfirmed
}

// codeReason is "CODE:n/m" for the code most nodes give (the first by name on a tie), where n counts all the nodes that fail.
func codeReason(byCode map[string]int, total int) string {
	failing, best, bestN := 0, "", 0
	for code, n := range byCode {
		failing += n
		if n > bestN || n == bestN && code < best {
			best, bestN = code, n
		}
	}
	if failing == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d/%d", best, failing, total)
}

// verifyRelay is the reason console cables cannot be served: "" when every lab node that is ready for pods, and at least one, says the
// relay of its node is ready (the node-agent drives the lines and the device access service of the node answers on cable.sock). Unlike
// the lifecycle features, no ready node is not "nothing to ask": without a relay on a node there is no line.
func verifyRelay(nodes []corev1.Node, selector map[string]string, tolerations []corev1.Toleration) string {
	checked := 0
	byCode := map[string]int{}
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[names.LabelNodeAgentReady] != "true" || !nodecap.Schedulable(n, selector, tolerations) {
			continue
		}
		checked++
		v, ok := n.Annotations[names.AnnotationConsoleRelay]
		if code := nodeagent.RelayCodeOf(v, ok); code != "" {
			byCode[code]++
		}
	}
	if checked == 0 {
		return nodeagent.CodeNodeUnconfirmed + ":0/0"
	}
	return codeReason(byCode, checked)
}

// nodeVerdict reads the nodes for the features the chart configured.
func (h *Handler) nodeVerdict(ctx context.Context, wantPersistence, wantObservation, wantRelay bool) (nodeVerdict, error) {
	if !wantPersistence && !wantObservation && !wantRelay {
		return nodeVerdict{}, nil
	}
	list, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nodeVerdict{}, err
	}
	v := verifyNodes(list.Items, h.labSelector, h.labTolerations, wantPersistence, wantObservation)
	if wantRelay {
		v.relayAsked, v.relayReason = true, verifyRelay(list.Items, h.labSelector, h.labTolerations)
	}
	return v, nil
}

// applyNodeSupport holds back, in the answer, what the nodes do not confirm.
func applyNodeSupport(base *protobuf.FeaturesResponse, v nodeVerdict) {
	// console cables need the relay of the nodes, whatever the chart says (the feature has no reason field: the detail is on the Nodes)
	if v.relayAsked && v.relayReason != "" && base.ConsoleCables != nil {
		base.ConsoleCables.Enabled = false
	}
	if v.checked == 0 {
		return
	}
	if v.persistenceReason != "" {
		if base.StatePersistence != nil {
			base.StatePersistence.Available = false
			base.StatePersistence.UnavailableReason = v.persistenceReason
		}
		if base.Lifecycle != nil {
			// a required snapshot is written through the same snapshot engine and registry as a restart from state
			base.Lifecycle.RetainedRestart = false
			base.Lifecycle.RequiredSnapshot = false
			base.Lifecycle.UnavailableReason = v.persistenceReason
		}
	}
	// the observation holds back every capability, so its reason is the one a caller needs to see first
	if v.observationReason != "" && base.Lifecycle != nil {
		l := base.Lifecycle
		l.PerLabStop, l.RequiredSnapshot, l.ConfirmedRuntime, l.RetainedRestart, l.FullGroupStop = false, false, false, false, false
		l.UnavailableReason = v.observationReason
	}
}

// lifecycleConfigured says some lifecycle capability is on in the configuration, which needs the native observation of the nodes.
func lifecycleConfigured(l *protobuf.LifecycleFeature) bool {
	return l != nil && (l.PerLabStop || l.RequiredSnapshot || l.ConfirmedRuntime || l.RetainedRestart || l.FullGroupStop)
}
