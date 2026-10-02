package nodecap

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func roomNode(name, cpu, mem string, ready bool) corev1.Node {
	labels := map[string]string{}
	if ready {
		labels["laboratory.cybericebox.com/node-agent-ready"] = "true"
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func roomPod(node, cpu, mem string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// Per-node room: only nodes with a ready node-agent count, free is allocatable less the platform reserve less the
// requests of the unfinished pods of the node, never negative.
func TestRoomsPerSchedulableNode(t *testing.T) {
	selector := map[string]string{"laboratory.cybericebox.com/node-agent-ready": "true"}
	nodes := []corev1.Node{roomNode("a", "4", "8Gi", true), roomNode("b", "2", "4Gi", true), roomNode("c", "8", "16Gi", false)}
	pods := []corev1.Pod{
		roomPod("a", "1", "1Gi", corev1.PodRunning),
		roomPod("a", "500m", "512Mi", corev1.PodPending),
		roomPod("a", "2", "2Gi", corev1.PodSucceeded), // finished: holds nothing
		roomPod("b", "3", "1Gi", corev1.PodRunning),   // overcommitted CPU
		roomPod("c", "1", "1Gi", corev1.PodRunning),
	}
	got := Rooms(nodes, pods, selector, nil, Reserve{Node: Amount{CPU: 500, Memory: 1 << 30}, Percent: 10})
	if len(got) != 2 {
		t.Fatalf("rooms = %+v, want the two ready nodes", got)
	}
	// a: (4000-500)*0.9 = 3150 m, (8Gi-1Gi)*0.9 = 6.3Gi
	a := got[0]
	if a.Name != "a" || a.Allocatable.CPU != 3150 || a.Allocatable.Memory != (7<<30)-(7<<30)/10 {
		t.Errorf("a allocatable = %+v", a)
	}
	if a.Free.CPU != 3150-1500 || a.Free.Memory != a.Allocatable.Memory-(1<<30)-(512<<20) {
		t.Errorf("a free = %+v", a.Free)
	}
	if b := got[1]; b.Free.CPU != 0 || b.Free.Memory <= 0 {
		t.Errorf("b free = %+v: CPU is overcommitted (zero), memory is not", b.Free)
	}
}

func TestPodRequestsTakesTheLargerOfInitAndContainers(t *testing.T) {
	p := roomPod("a", "100m", "64Mi", corev1.PodRunning)
	p.Spec.InitContainers = []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")}}}}
	p.Spec.Overhead = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("10Mi")}
	if got := PodRequests(&p); got.CPU != 300 || got.Memory != (64+10)<<20 {
		t.Errorf("PodRequests = %+v", got)
	}
}

func TestLargestDeviceIsPerResourceAndEmptyForNoNode(t *testing.T) {
	got := LargestDevice([]NodeRoom{
		{Name: "a", Allocatable: Amount{CPU: 4000, Memory: 8 << 30}},
		{Name: "b", Allocatable: Amount{CPU: 2000, Memory: 16 << 30}},
	})
	if got != (Amount{CPU: 4000, Memory: 16 << 30}) {
		t.Errorf("LargestDevice = %+v", got)
	}
	if got := LargestDevice(nil); got != (Amount{}) {
		t.Errorf("no node = %+v", got)
	}
}
