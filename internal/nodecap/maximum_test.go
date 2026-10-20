package nodecap

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	labSel = map[string]string{"cybericebox.com/lab": "true", "laboratory.cybericebox.com/node-agent-ready": "true"}
	labTol = []corev1.Toleration{{Key: "cybericebox.com/lab", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}
)

// labNode is a schedulable lab node of 4 CPU and 16Gi; group "" is a fixed node.
func labNode(name, group string) corev1.Node {
	n := roomNode(name, "4", "16Gi", true)
	n.Labels["cybericebox.com/lab"] = "true"
	if group != "" {
		n.Labels[LabelNodeGroup] = group
	}
	n.Spec.Taints = []corev1.Taint{{Key: "cybericebox.com/lab", Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	return n
}

func dsPod(node, cpu, mem string) corev1.Pod {
	p := roomPod(node, cpu, mem, corev1.PodRunning)
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "cilium"}}
	return p
}

// The budget covers the autoscaled nodes only and is counted as infra counts it: a fixed node takes nothing off it, an
// autoscaled one its allocatable less its DaemonSet pods. A platform pod that is not a DaemonSet's takes nothing off it (the
// budget does not count it either), so a cluster at its budget leaves no phantom virtual room.
func TestVirtualIsBudgetLessAutoscaledRoom(t *testing.T) {
	nodes := []corev1.Node{labNode("cp-1", ""), labNode("w1", "m7i-xlarge"), labNode("w2", "m7i-xlarge"), labNode("w3", "m7i-xlarge")}
	pods := []corev1.Pod{dsPod("w1", "500m", "1Gi"), dsPod("w2", "500m", "1Gi"), dsPod("w3", "500m", "1Gi"), dsPod("cp-1", "500m", "1Gi"),
		roomPod("w1", "1", "1Gi", corev1.PodRunning), dsPod("w1", "1", "1Gi")}
	pods[len(pods)-1].Status.Phase = corev1.PodSucceeded
	budget := Maximum{Budget: Amount{CPU: 3 * 3500, Memory: 3 * 15 << 30}}
	if got := budget.Virtual(nodes, pods, labSel, labTol, Reserve{}, PackingReserve{}); got != (Amount{}) {
		t.Fatalf("at the budget the virtual room is %+v, want none", got)
	}
	if got := budget.Remaining(nodes[:2], pods, labSel, labTol); got != (Amount{CPU: 2 * 3500, Memory: 2 * 15 << 30}) {
		t.Fatalf("one autoscaled node of three: %+v", got)
	}
}

// Every autoscaled node of the pool counts against the budget, schedulable or not (booting, without its node-agent, cordoned,
// NotReady, removed by an autoscaler): the autoscaler counts them in its totals.
func TestVirtualCountsEveryAutoscaledNodeOfThePool(t *testing.T) {
	notReady := labNode("not-ready", "g")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	cordoned := labNode("cordoned", "g")
	cordoned.Spec.Unschedulable = true
	removing := labNode("removing", "g")
	removing.Spec.Taints = append(removing.Spec.Taints, corev1.Taint{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule})
	booting := labNode("booting", "g")
	delete(booting.Labels, "laboratory.cybericebox.com/node-agent-ready")
	other := labNode("other-pool", "g")
	other.Labels["cybericebox.com/lab"] = "false"
	small := Maximum{Budget: Amount{CPU: 24000, Memory: 96 << 30}}
	got := small.Virtual([]corev1.Node{labNode("ok", "g"), notReady, cordoned, removing, booting, other}, nil, labSel, labTol, Reserve{}, PackingReserve{})
	if want := (Amount{CPU: 4000, Memory: 16 << 30}); got != want {
		t.Fatalf("virtual = %+v, want %+v (five nodes of the pool count, the other pool not)", got, want)
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
