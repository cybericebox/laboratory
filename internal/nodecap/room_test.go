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
	a, b := roomNode("a", "4", "8Gi", true), roomNode("b", "2", "16Gi", true)
	selector := map[string]string{"laboratory.cybericebox.com/node-agent-ready": "true"}
	got := LargestDevice([]corev1.Node{a, b}, selector, nil, Reserve{}, PackingReserve{})
	if got != (Amount{CPU: 4000, Memory: 16 << 30}) {
		t.Errorf("LargestDevice = %+v", got)
	}
	if got := LargestDevice(nil, selector, nil, Reserve{}, PackingReserve{}); got != (Amount{}) {
		t.Errorf("no node = %+v", got)
	}
}

// The largest device is net of the platform reserve and the packing reserve with no load, and counts the cordoned,
// NotReady and rebooting (no node-agent label) nodes of the pool, not the nodes outside it.
func TestLargestDeviceIsLoadFreeNetOfPackingOverThePool(t *testing.T) {
	selector := map[string]string{"pool": "labs", "laboratory.cybericebox.com/node-agent-ready": "true"}
	inPool := func(n corev1.Node) corev1.Node { n.Labels["pool"] = "labs"; return n }
	cordoned := inPool(roomNode("big", "8", "16Gi", true))
	cordoned.Spec.Unschedulable = true
	cordoned.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}}
	notReady := inPool(roomNode("mid", "4", "32Gi", true))
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	notReady.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute}}
	rebooting := inPool(roomNode("reboot", "6", "8Gi", false)) // node-agent down: the label is gone
	rebooting.Status.Conditions[0].Status = corev1.ConditionUnknown
	outside := roomNode("other", "64", "256Gi", true)
	nodes := []corev1.Node{cordoned, notReady, rebooting, outside, inPool(roomNode("small", "2", "4Gi", true))}
	reserve, packing := Reserve{Node: Amount{CPU: 500}, Percent: 10}, PackingReserve{Percent: 20}
	got := LargestDevice(nodes, selector, nil, reserve, packing)
	mem := int64(32 << 30)
	mem -= mem * 10 / 100
	mem -= mem * 20 / 100
	if got != (Amount{CPU: 5400, Memory: mem}) { // (8000-500) less 10% less 20%
		t.Errorf("LargestDevice = %+v, want 5400 / %d", got, mem)
	}
	if limits := DeviceLimits(nodes, selector, nil, reserve, packing); len(limits) != 4 || limits[2].CPU != 3960 { // (6000-500) less 10% less 20%
		t.Errorf("DeviceLimits = %+v", limits)
	}
	// Placement still needs the label: the rebooting node is in the pool, not schedulable.
	if Schedulable(&rebooting, selector, nil) || !InPool(&rebooting, selector, nil) {
		t.Error("a rebooting node is in the pool but takes no pod")
	}
}

func TestPackingReserveApply(t *testing.T) {
	cases := []struct {
		name     string
		r        PackingReserve
		cpu, mem int64
		wantCPU  int64
		wantMem  int64
	}{
		{"percent", PackingReserve{Percent: 15}, 3900, 1000, 3315, 850},
		{"fixed wins", PackingReserve{Percent: 15, CPU: 500, Memory: 100}, 3900, 1000, 3400, 900},
		{"fixed on one resource", PackingReserve{Percent: 10, CPU: 500}, 3900, 1000, 3400, 900},
		{"never negative", PackingReserve{CPU: 500}, 300, 0, 0, 0},
		{"none", PackingReserve{}, 3900, 1000, 3900, 1000},
	}
	for _, c := range cases {
		if cpu, mem := c.r.Apply(c.cpu, c.mem); cpu != c.wantCPU || mem != c.wantMem {
			t.Errorf("%s: %d / %d, want %d / %d", c.name, cpu, mem, c.wantCPU, c.wantMem)
		}
	}
}
