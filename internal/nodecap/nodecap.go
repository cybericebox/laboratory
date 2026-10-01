// Package nodecap holds the node eligibility rule shared by the operator's scheduler and the
// management agent: which nodes lab pods can run on.
package nodecap

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
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
