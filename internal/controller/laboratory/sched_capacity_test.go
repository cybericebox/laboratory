package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
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

func TestSnapshotCapacityRoomsAreAllocatableMinusRequests(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi"), testNode("n2", "4", "8Gi")}
	pods := []corev1.Pod{testPod("n1", "1", "1Gi"), testPod("n1", "500m", "1Gi"), testPod("n2", "2", "2Gi")}
	done := testPod("n2", "1", "1Gi")
	done.Status.Phase = corev1.PodSucceeded
	pending := testPod("", "3", "3Gi")
	pods = append(pods, done, pending)

	c := snapshotCapacity(nodes, pods, nil, nil, amount{}, 0, nodecap.PackingReserve{})
	if c.nodes != 2 || c.allocatable != (amount{8000, 16 << 30}) {
		t.Fatalf("capacity = %+v", c)
	}
	if r := c.rooms(); len(r) != 2 || r[0] != room("n1", 2500, 6<<30) || r[1] != room("n2", 2000, 6<<30) {
		t.Fatalf("rooms = %+v (finished and unscheduled pods hold nothing)", r)
	}
}

// An overcommitted node must not hide the room on another one.
func TestSnapshotCapacityClampsPerNode(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "1", "1Gi"), testNode("n2", "4", "4Gi")}
	pods := []corev1.Pod{testPod("n1", "3", "3Gi")}
	c := snapshotCapacity(nodes, pods, nil, nil, amount{}, 0, nodecap.PackingReserve{})
	if r := c.rooms(); len(r) != 2 || r[0] != room("n1", 0, 0) || r[1] != room("n2", 4000, 4<<30) {
		t.Fatalf("rooms = %+v", r)
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

	c := snapshotCapacity(nodes, nil, sel, nil, amount{}, 0, nodecap.PackingReserve{})
	if c.nodes != 2 { // ready + soft (PreferNoSchedule does not exclude)
		t.Fatalf("eligible nodes = %d, want 2", c.nodes)
	}
	tol := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "lab", Effect: corev1.TaintEffectNoSchedule}}
	if c := snapshotCapacity(nodes, nil, sel, tol, amount{}, 0, nodecap.PackingReserve{}); c.nodes != 3 {
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

// The platform reserve is taken after the kubelet's (allocatable is already net of it) and after every pod
// scheduled on the node, the proxy and DaemonSet pods included, in both proxy modes; the absolute per-node
// reserve comes off each node before the percentage is applied.
func TestPlatformReserveCountsProxyAndNodeReserve(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi"), testNode("n2", "4", "8Gi")}
	// deployment mode: a proxy pod on each of two nodes; daemonset mode would be the same on every node.
	proxy := []corev1.Pod{testPod("n1", "750m", "128Mi"), testPod("n2", "750m", "128Mi")}
	c := snapshotCapacity(nodes, proxy, nil, nil, amount{cpu: 500, mem: 1 << 30}, 0, nodecap.PackingReserve{})
	want := room("n1", 4000-500-750, 8<<30-1<<30-128<<20)
	if r := c.rooms(); c.allocatable != (amount{cpu: 7000, mem: 14 << 30}) || len(r) != 2 || r[0] != want || r[1] != room("n2", want.CPU, want.Memory) {
		t.Fatalf("capacity = %+v, rooms = %+v", c, r)
	}
	// a node smaller than the reserve gives nothing, never a negative amount
	small := snapshotCapacity([]corev1.Node{testNode("n1", "100m", "1Gi")}, nil, nil, nil, amount{cpu: 500, mem: 2 << 30}, 0, nodecap.PackingReserve{})
	if r := small.rooms(); small.allocatable != (amount{}) || len(r) != 1 || r[0] != room("n1", 0, 0) {
		t.Fatalf("small = %+v", small)
	}
}

// The room of a node is its allocatable less the platform node reserve and the requests of the pods that are not lab pods,
// less the packing reserve taken from what is left. Lab pods (and the VPN and gateway pods) take nothing from it.
func TestRoomsSubtractNonLabAndPacking(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi"), testNode("n2", "4", "8Gi")}
	cilium := testPod("n1", "100m", "256Mi")
	labPod := testPod("n1", "1", "1Gi")
	labPod.Labels = map[string]string{names.LabelLab: "lab", names.LabelDevice: "dev"}
	gateway := testPod("n2", "500m", "512Mi")
	gateway.Labels = map[string]string{names.LabelComponent: names.ComponentGateway}
	done := testPod("n2", "1", "1Gi")
	done.Status.Phase = corev1.PodSucceeded
	c := snapshotCapacity(nodes, []corev1.Pod{cilium, labPod, gateway, done}, nil, nil, amount{}, 0, nodecap.PackingReserve{Percent: 15})
	// n1: (4000-100) - 585 = 3315; (8Gi-256Mi) = 8321499136, less 1248224870 = 7073274266. n2: 4000 - 600 = 3400, 8Gi less 1288490188.
	want := []nodeRoom{{"n1", 3315, 7073274266}, {"n2", 3400, 8589934592 - 8589934592*15/100}}
	got := c.rooms()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rooms = %+v, want %+v", got, want)
	}
}

// A fixed packing reserve replaces the percent for its resource only.
func TestPackingFixedWinsOverPercent(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi")}
	c := snapshotCapacity(nodes, nil, nil, nil, amount{}, 0, nodecap.PackingReserve{Percent: 15, CPU: 500})
	want := nodeRoom{"n1", 3500, 8<<30 - (8<<30)*15/100}
	if got := c.rooms(); len(got) != 1 || got[0] != want {
		t.Fatalf("rooms = %+v, want %+v", got, want)
	}
	c = snapshotCapacity(nodes, nil, nil, nil, amount{}, 0, nodecap.PackingReserve{Percent: 15, CPU: 500, Memory: 1 << 30})
	want = nodeRoom{"n1", 3500, 7 << 30}
	if got := c.rooms(); len(got) != 1 || got[0] != want {
		t.Fatalf("rooms = %+v, want %+v", got, want)
	}
}

// The percent platform reserve comes off every node before the non-lab requests: 4000m/8Gi, platform 10 %, Cilium 100m/256Mi,
// packing 15 %: (4000-400-100)*0.85 = 2975m, (8Gi-858993459-256Mi)*0.85.
func TestRoomsSubtractPlatformPercentPerNode(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi")}
	c := snapshotCapacity(nodes, []corev1.Pod{testPod("n1", "100m", "256Mi")}, nil, nil, amount{}, 10, nodecap.PackingReserve{Percent: 15})
	mem := int64(8<<30) - int64(8<<30)*10/100 - 256<<20
	want := nodeRoom{"n1", 2975, mem - mem*15/100}
	if got := c.rooms(); len(got) != 1 || got[0] != want {
		t.Fatalf("rooms = %+v, want %+v", got, want)
	}
}

// freeRooms takes the bound lab pods off the rooms, the held pods of stopped labs included; a lab pod with no node is not
// counted.
func TestFreeRoomsSubtractBoundLabPodsAndStopped(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "4", "8Gi"), testNode("n2", "4", "8Gi")}
	lab := func(node, cpu, mem string) corev1.Pod {
		p := testPod(node, cpu, mem)
		p.Labels = map[string]string{names.LabelLab: "lab", names.LabelDevice: "dev"}
		return p
	}
	held := lab("n2", "500m", "1Gi")
	held.Name = "held-dev"
	pending := lab("", "2", "2Gi")
	done := lab("n1", "1", "1Gi")
	done.Status.Phase = corev1.PodFailed
	pods := []corev1.Pod{testPod("n1", "100m", "256Mi"), lab("n1", "1", "1Gi"), held, pending, done}
	c := snapshotCapacity(nodes, pods, nil, nil, amount{}, 0, nodecap.PackingReserve{})
	want := []nodeRoom{{"n1", 4000 - 100 - 1000, 8<<30 - 256<<20 - 1<<30}, {"n2", 3500, 7 << 30}}
	got := c.freeRooms()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("freeRooms = %+v, want %+v", got, want)
	}
	if r := c.rooms(); r[0].CPU != 3900 || r[1].CPU != 4000 {
		t.Errorf("rooms must stay before lab usage: %+v", r)
	}
}

// maxNode is the largest usable node, whatever runs there.
func TestSnapshotMaxNode(t *testing.T) {
	nodes := []corev1.Node{testNode("a", "4", "4Gi"), testNode("b", "2", "8Gi")}
	c := snapshotCapacity(nodes, nil, map[string]string{"lab": "yes"}, nil, amount{}, 10, nodecap.PackingReserve{})
	if c.maxNode.cpu != 3600 || c.maxNode.mem != 8*(1<<30)-8*(1<<30)/10 {
		t.Fatalf("maxNode = %+v", c.maxNode)
	}
}

// maxNode is load-free and net of the packing reserve, and counts the cordoned and NotReady nodes of the pool: the same
// fixture as the agent's TestLargestDeviceMatchesTheSchedulersNeverFits, with the same answer.
func TestSnapshotMaxNodeIsNetOfPackingAndCountsDrainedNodes(t *testing.T) {
	big := testNode("big", "8", "16Gi")
	big.Spec.Unschedulable = true
	mid := testNode("mid", "4", "32Gi")
	mid.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	other := testNode("other", "64", "256Gi")
	other.Labels = nil
	small := testNode("small", "2", "4Gi")
	nodes := []corev1.Node{big, mid, other, small}
	pods := []corev1.Pod{testPod("small", "1", "1Gi")} // load does not change it
	c := snapshotCapacity(nodes, pods, map[string]string{"lab": "yes"}, nil, amount{cpu: 500}, 10, nodecap.PackingReserve{Percent: 20})
	mem := int64(32 << 30)
	mem -= mem * 10 / 100
	mem -= mem * 20 / 100
	if c.maxNode != (amount{cpu: 5400, mem: mem}) || c.nodes != 1 {
		t.Fatalf("maxNode = %+v, nodes = %d", c.maxNode, c.nodes)
	}
	// Every node drained: no schedulable node, so nothing is failed and the scheduler waits (NoSchedulableNodes).
	small.Spec.Unschedulable = true
	c = snapshotCapacity([]corev1.Node{big, small}, nil, map[string]string{"lab": "yes"}, nil, amount{cpu: 500}, 10, nodecap.PackingReserve{Percent: 20})
	if c.nodes != 0 || len(c.rooms()) != 0 {
		t.Fatalf("nodes = %d, rooms = %+v", c.nodes, c.rooms())
	}
}
