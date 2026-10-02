package nodecap

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// C-18: the lab node selector carries the node-agent-ready label. A controller+worker node (soft control-plane taint) takes labs once its
// node-agent has marked it, a worker whose node-agent is down does not, and a hard-tainted control-plane node needs the toleration.
func TestSchedulableNeedsTheNodeAgentLabel(t *testing.T) {
	selector := map[string]string{"laboratory.cybericebox.com/node-agent-ready": "true"}
	node := func(labels map[string]string, taints ...corev1.Taint) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       corev1.NodeSpec{Taints: taints},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		}
	}
	ready := map[string]string{"laboratory.cybericebox.com/node-agent-ready": "true"}
	soft := corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectPreferNoSchedule}
	hard := corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}
	tolerate := []corev1.Toleration{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists}}

	for name, tc := range map[string]struct {
		node *corev1.Node
		tols []corev1.Toleration
		want bool
	}{
		"worker with a ready node-agent":                {node(ready), nil, true},
		"worker whose node-agent is down":               {node(nil), nil, false},
		"controller+worker (soft taint), node-agent up": {node(ready, soft), nil, true},
		"controller+worker, node-agent not there":       {node(nil, soft), nil, false},
		"hard-tainted control plane, not tolerated":     {node(ready, hard), nil, false},
		"hard-tainted control plane, tolerated":         {node(ready, hard), tolerate, true},
	} {
		if got := Schedulable(tc.node, selector, tc.tols); got != tc.want {
			t.Errorf("%s: Schedulable = %v, want %v", name, got, tc.want)
		}
	}
}
