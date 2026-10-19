package nodecap

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

var (
	labSel = map[string]string{"cybericebox.io/lab": "true", "laboratory.cybericebox.com/node-agent-ready": "true"}
	labTol = []corev1.Toleration{{Key: "cybericebox.io/lab", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}
)

// labNode is a schedulable lab node of 4 CPU and 16Gi; group "" is a fixed node.
func labNode(name, group string) corev1.Node {
	n := roomNode(name, "4", "16Gi", true)
	n.Labels["cybericebox.io/lab"] = "true"
	if group != "" {
		n.Labels[LabelNodeGroup] = group
	}
	n.Spec.Taints = []corev1.Taint{{Key: "cybericebox.io/lab", Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	return n
}

var budget = Maximum{Budget: Amount{CPU: 12000, Memory: 48 << 30}}

// The budget covers the autoscaled nodes only: a fixed node takes nothing off it, an autoscaled one its room (allocatable less
// the non-lab pods), and what the budget leaves is virtual.
func TestVirtualIsBudgetLessAutoscaledRoom(t *testing.T) {
	nodes := []corev1.Node{labNode("cp-1", ""), labNode("w1", "m7i-xlarge")}
	pods := []corev1.Pod{roomPod("w1", "500m", "1Gi", corev1.PodRunning), roomPod("w1", "1", "1Gi", corev1.PodSucceeded)}
	got := budget.Virtual(nodes, pods, labSel, labTol, Reserve{}, PackingReserve{})
	if want := (Amount{CPU: 12000 - 3500, Memory: 48<<30 - 15<<30}); got != want {
		t.Fatalf("virtual = %+v, want %+v", got, want)
	}
}

// A node that is not schedulable (booting, without its node-agent, cordoned, NotReady, removed by an autoscaler) takes nothing
// off the budget: its room is still to come.
func TestVirtualCountsOnlySchedulableAutoscaledNodes(t *testing.T) {
	notReady := labNode("not-ready", "g")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	cordoned := labNode("cordoned", "g")
	cordoned.Spec.Unschedulable = true
	removing := labNode("removing", "g")
	removing.Spec.Taints = append(removing.Spec.Taints, corev1.Taint{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule})
	booting := labNode("booting", "g")
	delete(booting.Labels, "laboratory.cybericebox.com/node-agent-ready")
	got := budget.Virtual([]corev1.Node{labNode("ok", "g"), notReady, cordoned, removing, booting}, nil, labSel, labTol, Reserve{}, PackingReserve{})
	if want := (Amount{CPU: 8000, Memory: 32 << 30}); got != want {
		t.Fatalf("virtual = %+v, want %+v", got, want)
	}
}

// The operator's reserves are kept on the virtual room as on a real node: the platform percent, then the packing percent.
func TestVirtualRoomLessReserves(t *testing.T) {
	got := Maximum{Budget: Amount{CPU: 10000, Memory: 10 << 30}}.Virtual(nil, nil, labSel, labTol, Reserve{Percent: 10}, PackingReserve{Percent: 20})
	mem := int64(9 << 30)
	if want := (Amount{CPU: 7200, Memory: mem - mem*20/100}); got != want {
		t.Fatalf("virtual = %+v, want %+v", got, want)
	}
}

// Autoscaled nodes beyond the budget leave no virtual room, never a negative one.
func TestMaximumNeverBelowCurrent(t *testing.T) {
	small := Maximum{Budget: Amount{CPU: 4000, Memory: 8 << 30}}
	if got := small.Virtual([]corev1.Node{labNode("a", "g"), labNode("b", "g")}, nil, labSel, labTol, Reserve{}, PackingReserve{}); got != (Amount{}) {
		t.Fatalf("virtual = %+v", got)
	}
}

func TestMaximumZeroIsCurrent(t *testing.T) {
	if got := (Maximum{}).Virtual([]corev1.Node{labNode("a", "g")}, nil, labSel, labTol, Reserve{}, PackingReserve{}); got != (Amount{}) || (Maximum{}).Set() {
		t.Fatalf("no budget: %+v", got)
	}
	if m, err := ParseMaximum("", ""); err != nil || m.Set() {
		t.Fatalf("ParseMaximum of nothing = %+v %v", m, err)
	}
}

func TestParseMaximum(t *testing.T) {
	m, err := ParseMaximum("48", "192Gi")
	if err != nil || m.Budget != (Amount{CPU: 48000, Memory: 192 << 30}) {
		t.Fatalf("ParseMaximum = %+v %v", m, err)
	}
	for _, bad := range [][2]string{{"48", ""}, {"", "1Gi"}, {"x", "1Gi"}, {"0", "1Gi"}, {"1", "-1Gi"}} {
		if _, err := ParseMaximum(bad[0], bad[1]); err == nil {
			t.Errorf("ParseMaximum(%v) must fail", bad)
		}
	}
}
