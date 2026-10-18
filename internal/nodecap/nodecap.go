// Package nodecap holds the node eligibility rule shared by the operator's scheduler and the
// management agent: which nodes lab pods can run on.
package nodecap

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/cybericebox/laboratory/internal/names"
)

// Schedulable reports whether lab pods can be placed on the node: it is
// schedulable and Ready, matches the lab node selector and every NoSchedule or
// NoExecute taint is tolerated by the lab tolerations.
func Schedulable(node *corev1.Node, selector map[string]string, tolerations []corev1.Toleration) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			return false
		}
	}
	for k, v := range selector {
		if node.Labels[k] != v {
			return false
		}
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for j := range tolerations {
			if tolerations[j].ToleratesTaint(klog.Background(), taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// InPool reports whether the node is one of the lab nodes whatever its state now: it matches the lab node selector and the
// lab tolerations, while being cordoned, NotReady, without its node-agent (the node-agent-ready label is a readiness
// signal, removed while the node-agent is down) or carrying a taint that only says so (not ready, unreachable, pressure,
// network or CNI not up yet) is ignored. A drained or rebooting node is still part of the pool. Placement itself still
// needs Schedulable, the label included.
func InPool(node *corev1.Node, selector map[string]string, tolerations []corev1.Toleration) bool {
	membership := make(map[string]string, len(selector))
	for k, v := range selector {
		if k != names.LabelNodeAgentReady {
			membership[k] = v
		}
	}
	n := node.DeepCopy()
	n.Spec.Unschedulable = false
	n.Status.Conditions = nil
	n.Spec.Taints = nil
	for _, taint := range node.Spec.Taints {
		switch taint.Key {
		case corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable, corev1.TaintNodeUnschedulable, corev1.TaintNodeMemoryPressure, corev1.TaintNodeDiskPressure, corev1.TaintNodePIDPressure, corev1.TaintNodeNetworkUnavailable, "node.cilium.io/agent-not-ready":
			// These express temporary availability/bootstrap, not pool membership.
			continue
		}
		n.Spec.Taints = append(n.Spec.Taints, taint)
	}
	return Schedulable(n, membership, tolerations)
}
