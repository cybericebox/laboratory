package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNode(name, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"lab": "yes"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		},
	}
}

func testPod(node, cpu, mem string) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
			}},
		}}},
	}
}

func TestSnapshotCapacityFreeIsAllocatableMinusRequests(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi"), testNode("n2", "4", "8Gi")}
	pods := []corev1.Pod{testPod("n1", "1", "1Gi"), testPod("n1", "500m", "1Gi"), testPod("n2", "2", "2Gi")}
	done := testPod("n2", "1", "1Gi")
	done.Status.Phase = corev1.PodSucceeded
	pending := testPod("", "3", "3Gi")
	pods = append(pods, done, pending)

	c := snapshotCapacity(nodes, pods, nil, nil)
	if c.nodes != 2 || c.allocatable != (amount{8000, 16 << 30}) {
		t.Fatalf("capacity = %+v", c)
	}
	if c.free != (amount{cpu: 8000 - 3500, mem: (16 - 4) << 30}) {
		t.Fatalf("free = %+v (finished and unscheduled pods hold nothing)", c.free)
	}
}

// An overcommitted node must not hide the room on another one.
func TestSnapshotCapacityClampsPerNode(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "1", "1Gi"), testNode("n2", "4", "4Gi")}
	pods := []corev1.Pod{testPod("n1", "3", "3Gi")}
	c := snapshotCapacity(nodes, pods, nil, nil)
	if c.free != (amount{4000, 4 << 30}) {
		t.Fatalf("free = %+v", c.free)
	}
}

func TestSnapshotCapacityNodeEligibility(t *testing.T) {
	ready := testNode("ready", "1", "1Gi")
	cordoned := testNode("cordoned", "1", "1Gi")
	cordoned.Spec.Unschedulable = true
	notReady := testNode("notready", "1", "1Gi")
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	other := testNode("other", "1", "1Gi")
	other.Labels = nil
	tainted := testNode("tainted", "1", "1Gi")
	tainted.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "lab", Effect: corev1.TaintEffectNoSchedule}}
	soft := testNode("soft", "1", "1Gi")
	soft.Spec.Taints = []corev1.Taint{{Key: "x", Effect: corev1.TaintEffectPreferNoSchedule}}
	nodes := []corev1.Node{ready, cordoned, notReady, other, tainted, soft}
	sel := map[string]string{"lab": "yes"}

	c := snapshotCapacity(nodes, nil, sel, nil)
	if c.nodes != 2 { // ready + soft (PreferNoSchedule does not exclude)
		t.Fatalf("eligible nodes = %d, want 2", c.nodes)
	}
	tol := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "lab", Effect: corev1.TaintEffectNoSchedule}}
	if c := snapshotCapacity(nodes, nil, sel, tol); c.nodes != 3 {
		t.Fatalf("with the toleration nodes = %d, want 3", c.nodes)
	}
}

func TestPodRequestsInitAndOverhead(t *testing.T) {
	p := testPod("n", "1", "1Gi")
	p.Spec.InitContainers = []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("100Mi"),
	}}}}
	p.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	if got := podRequests(&p); got != (amount{cpu: 2100, mem: 1 << 30}) {
		t.Fatalf("requests = %+v", got)
	}
}

func TestCapacityCheck(t *testing.T) {
	c := capacity{nodes: 2, allocatable: amount{cpu: 10000, mem: 10 << 30}, free: amount{cpu: 3000, mem: 3 << 30}}
	// 10% headroom keeps 1 CPU and 1Gi free: 2 CPU / 2Gi may be taken.
	cases := []struct {
		name string
		need amount
		want fit
	}{
		{"nothing needed", amount{}, fitOK},
		{"fits exactly", amount{2000, 2 << 30}, fitOK},
		{"cpu over the headroom", amount{2001, 1 << 30}, fitWait},
		{"memory over the headroom", amount{1000, 2<<30 + 1}, fitWait},
		{"larger than the cluster", amount{9001, 1 << 30}, fitNever},
		{"memory larger than the cluster", amount{1, 9<<30 + 1}, fitNever},
	}
	for _, tc := range cases {
		if got := c.check(tc.need, 10); got != tc.want {
			t.Errorf("%s: check = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := (capacity{}).check(amount{cpu: 1}, 10); got != fitNoNodes {
		t.Errorf("no nodes: %d", got)
	}
	if got := (capacity{}).check(amount{}, 10); got != fitOK {
		t.Errorf("no nodes but nothing needed: %d", got)
	}
	c.take(amount{2000, 2 << 30})
	if c.free != (amount{1000, 1 << 30}) {
		t.Errorf("free after take = %+v", c.free)
	}
	c.take(amount{5000, 5 << 30})
	if c.free != (amount{}) {
		t.Errorf("free must not go negative: %+v", c.free)
	}
}
