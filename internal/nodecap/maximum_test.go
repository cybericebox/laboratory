package nodecap

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	labSel = map[string]string{"cybericebox.io/lab": "true", "laboratory.cybericebox.com/node-agent-ready": "true"}
	labTol = []corev1.Toleration{{Key: "cybericebox.io/lab", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}
)

func labNode(name string) corev1.Node {
	n := roomNode(name, "4", "16Gi", true)
	n.Labels["cybericebox.io/lab"] = "true"
	n.Spec.Taints = []corev1.Taint{{Key: "cybericebox.io/lab", Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	return n
}

func daemonSet(name, cpu, mem string, mutate func(*corev1.PodSpec)) appsv1.DaemonSet {
	ds := appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ds.Spec.Template.Spec.Containers = []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
	}}}}
	if mutate != nil {
		mutate(&ds.Spec.Template.Spec)
	}
	return ds
}

var tolerateAll = func(s *corev1.PodSpec) { s.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}} }

func TestVirtualCountsOnlySchedulableLabNodes(t *testing.T) {
	notReady := labNode("not-ready")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	cordoned := labNode("cordoned")
	cordoned.Spec.Unschedulable = true
	removing := labNode("removing")
	removing.Spec.Taints = append(removing.Spec.Taints, corev1.Taint{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule})
	booting := labNode("booting")
	delete(booting.Labels, "laboratory.cybericebox.com/node-agent-ready")
	nodes := []corev1.Node{labNode("ok"), notReady, cordoned, removing, booting}
	m := Maximum{Nodes: 5, Added: Amount{CPU: 4000, Memory: 16 << 30}}
	if n, _ := m.Virtual(nodes, nil, labSel, labTol, Reserve{}, PackingReserve{}); n != 4 {
		t.Fatalf("virtual = %d, want 4: only the schedulable lab node is current", n)
	}
}

// One added node gives the same room as a real node of that size with the same DaemonSet pods on it.
func TestVirtualRoomMatchesFreeForLabs(t *testing.T) {
	reserve := Reserve{Node: Amount{CPU: 200, Memory: 512 << 20}, Percent: 10}
	packing := PackingReserve{Percent: 15}
	dss := []appsv1.DaemonSet{daemonSet("cilium", "100m", "256Mi", tolerateAll), daemonSet("node-agent", "50m", "128Mi", tolerateAll)}
	real := labNode("real")
	pods := []corev1.Pod{roomPod("real", "100m", "256Mi", corev1.PodRunning), roomPod("real", "50m", "128Mi", corev1.PodRunning)}
	want := FreeForLabs([]corev1.Node{real}, pods, labSel, labTol, reserve, packing)[0]
	m := Maximum{Nodes: 3, Added: Amount{CPU: 4000, Memory: 16 << 30}}
	n, room := m.Virtual([]corev1.Node{real}, dss, labSel, labTol, reserve, packing)
	if n != 2 || room.CPU != want.CPU || room.Memory != want.Memory {
		t.Fatalf("virtual %d x %+v, want 2 x %+v", n, room, want)
	}
	rooms := VirtualRooms(n, room)
	if len(rooms) != 2 || rooms[0].Name != "virtual/0" || rooms[1].Name != "virtual/1" || rooms[1].CPU != room.CPU {
		t.Fatalf("rooms = %+v", rooms)
	}
}

func TestDaemonSetOverheadHonoursSelectorAffinityTolerations(t *testing.T) {
	dss := []appsv1.DaemonSet{
		daemonSet("everywhere", "100m", "100Mi", tolerateAll),
		daemonSet("lab-tolerant", "10m", "10Mi", func(s *corev1.PodSpec) {
			s.Tolerations = []corev1.Toleration{{Key: "cybericebox.io/lab", Operator: corev1.TolerationOpExists}}
			s.NodeSelector = map[string]string{"kubernetes.io/os": "linux"}
		}),
		daemonSet("not-tolerant", "1", "1Gi", nil),
		daemonSet("other-pool", "1", "1Gi", func(s *corev1.PodSpec) {
			tolerateAll(s)
			s.NodeSelector = map[string]string{"pool": "gpu"}
		}),
		daemonSet("affinity-in", "1m", "1Mi", func(s *corev1.PodSpec) {
			tolerateAll(s)
			s.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "cybericebox.io/lab", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}}}}}}}
		}),
		daemonSet("affinity-out", "1", "1Gi", func(s *corev1.PodSpec) {
			tolerateAll(s)
			s.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "cybericebox.io/lab", Operator: corev1.NodeSelectorOpDoesNotExist}}}}}}}
		}),
	}
	got := DaemonSetOverhead(dss, labSel, labTol)
	if want := (Amount{CPU: 111, Memory: 111 << 20}); got != want {
		t.Fatalf("overhead = %+v, want %+v", got, want)
	}
}

func TestMaximumNeverBelowCurrent(t *testing.T) {
	m := Maximum{Nodes: 2, Added: Amount{CPU: 4000, Memory: 16 << 30}}
	if n, _ := m.Virtual([]corev1.Node{labNode("a"), labNode("b"), labNode("c")}, nil, labSel, labTol, Reserve{}, PackingReserve{}); n != 0 {
		t.Fatalf("virtual = %d: more nodes than the maximum leave none virtual", n)
	}
}

func TestMaximumZeroIsCurrent(t *testing.T) {
	if n, room := (Maximum{}).Virtual(nil, nil, labSel, labTol, Reserve{}, PackingReserve{}); n != 0 || room != (NodeFree{}) {
		t.Fatalf("no maximum: %d x %+v", n, room)
	}
	if m, err := ParseMaximum(0, "", ""); err != nil || m != (Maximum{}) {
		t.Fatalf("ParseMaximum(0) = %+v %v", m, err)
	}
}

func TestParseMaximum(t *testing.T) {
	m, err := ParseMaximum(4, "3920m", "15Gi")
	if err != nil || m != (Maximum{Nodes: 4, Added: Amount{CPU: 3920, Memory: 15 << 30}}) {
		t.Fatalf("ParseMaximum = %+v %v", m, err)
	}
	for _, bad := range [][3]string{{"-1", "1", "1Gi"}, {"2", "", "1Gi"}, {"2", "1", ""}, {"2", "x", "1Gi"}, {"2", "0", "1Gi"}} {
		n := map[string]int{"-1": -1, "2": 2}[bad[0]]
		if _, err := ParseMaximum(n, bad[1], bad[2]); err == nil {
			t.Errorf("ParseMaximum(%v) must fail", bad)
		}
	}
}
