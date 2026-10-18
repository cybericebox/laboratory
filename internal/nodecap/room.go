package nodecap

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/cybericebox/laboratory/internal/names"
)

// Amount is a CPU and memory quantity: millicores and bytes.
type Amount struct {
	CPU, Memory int64
}

// Reserve is what the platform keeps free on the lab nodes (the scheduler's reserve): Node comes off every node's
// allocatable first, then Percent (0-99) of what is left of a node is kept free.
type Reserve struct {
	Node    Amount
	Percent int
}

// NodeRoom is the room of one schedulable lab node. Allocatable is what labs can ever use there (the node's
// allocatable less the platform reserve); Free is Allocatable less the requests of the pods running on the node.
type NodeRoom struct {
	Name        string
	Allocatable Amount
	Free        Amount
}

// PodRequests is the amount the scheduler reserves for a pod: the larger of the sum of its containers and its
// biggest init container, plus the pod overhead.
func PodRequests(pod *corev1.Pod) Amount {
	var sum, initMax Amount
	for i := range pod.Spec.Containers {
		r := pod.Spec.Containers[i].Resources.Requests
		sum.CPU += r.Cpu().MilliValue()
		sum.Memory += r.Memory().Value()
	}
	for i := range pod.Spec.InitContainers {
		r := pod.Spec.InitContainers[i].Resources.Requests
		if v := r.Cpu().MilliValue(); v > initMax.CPU {
			initMax.CPU = v
		}
		if v := r.Memory().Value(); v > initMax.Memory {
			initMax.Memory = v
		}
	}
	sum.CPU = max(sum.CPU, initMax.CPU) + pod.Spec.Overhead.Cpu().MilliValue()
	sum.Memory = max(sum.Memory, initMax.Memory) + pod.Spec.Overhead.Memory().Value()
	return sum
}

// Rooms is the room of every schedulable node, in the order of nodes. The requests of ALL pods scheduled on a node
// count (DaemonSets, the proxy and system pods included), finished pods hold nothing. The percentage reserve is
// taken per node here, where the scheduler takes it from the sum over the nodes; a device has to fit one node, so
// the per-node view is the stricter one.
func Rooms(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve) []NodeRoom {
	requested := map[string]Amount{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		r, sum := PodRequests(p), requested[p.Spec.NodeName]
		requested[p.Spec.NodeName] = Amount{sum.CPU + r.CPU, sum.Memory + r.Memory}
	}
	var out []NodeRoom
	for i := range nodes {
		n := &nodes[i]
		if !Schedulable(n, selector, tolerations) {
			continue
		}
		alloc := Amount{
			CPU:    max(n.Status.Allocatable.Cpu().MilliValue()-reserve.Node.CPU, 0),
			Memory: max(n.Status.Allocatable.Memory().Value()-reserve.Node.Memory, 0),
		}
		alloc.CPU -= alloc.CPU * int64(reserve.Percent) / 100
		alloc.Memory -= alloc.Memory * int64(reserve.Percent) / 100
		req := requested[n.Name]
		out = append(out, NodeRoom{
			Name: n.Name, Allocatable: alloc,
			Free: Amount{CPU: max(alloc.CPU-req.CPU, 0), Memory: max(alloc.Memory-req.Memory, 0)},
		})
	}
	return out
}

// DeviceLimits is, for every node of the pool (InPool: cordoned, NotReady and rebooting ones too, so a drained node does
// not shrink it), the most it gives one lab pod with nothing else on it: its allocatable less the platform reserve (as in
// Rooms) and less the packing reserve, which labs never use (see FreeForLabs). A pod that fits none of them, CPU and
// memory together, can never be placed.
func DeviceLimits(nodes []corev1.Node, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve, packing PackingReserve) []Amount {
	var out []Amount
	for i := range nodes {
		n := &nodes[i]
		if !InPool(n, selector, tolerations) {
			continue
		}
		cpu := max(n.Status.Allocatable.Cpu().MilliValue()-reserve.Node.CPU, 0)
		mem := max(n.Status.Allocatable.Memory().Value()-reserve.Node.Memory, 0)
		cpu -= cpu * int64(reserve.Percent) / 100
		mem -= mem * int64(reserve.Percent) / 100
		cpu, mem = packing.Apply(cpu, mem)
		out = append(out, Amount{CPU: cpu, Memory: mem})
	}
	return out
}

// LargestDevice is the largest device the lab nodes can ever hold, per resource: the largest of DeviceLimits for CPU and
// for memory, each on its own. It is an upper bound per resource: the two may come from different nodes, so a device that
// needs both at once may still fit no node (the operator then fails it, see DeviceLimits). Zero for no node.
func LargestDevice(nodes []corev1.Node, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve, packing PackingReserve) Amount {
	var out Amount
	for _, l := range DeviceLimits(nodes, selector, tolerations, reserve, packing) {
		out.CPU, out.Memory = max(out.CPU, l.CPU), max(out.Memory, l.Memory)
	}
	return out
}

// PackingReserve is the hidden share of a node that labs never use, because nodes do not share memory and devices leave gaps
// between them. Percent (0-49) is taken from what is free on the node; a fixed CPU or Memory (millicores, bytes) replaces the
// percent for that resource when it is non-zero.
type PackingReserve struct {
	Percent     int
	CPU, Memory int64
}

// Apply is what is left of free (the CPU and memory of a node after everything else) once the reserve is kept back. Never
// negative. The reserve is rounded down, so the result is an exact integer.
func (r PackingReserve) Apply(freeCPU, freeMem int64) (cpu, mem int64) {
	keep := func(free, fixed int64) int64 {
		if fixed == 0 {
			fixed = free * int64(r.Percent) / 100
		}
		return max(free-fixed, 0)
	}
	return keep(freeCPU, r.CPU), keep(freeMem, r.Memory)
}

// NodeFree is what labs can still take on one schedulable node.
type NodeFree struct {
	Name        string
	CPU, Memory int64
}

// IsLabPod reports whether the pod belongs to a lab: a device pod or the VPN or gateway pod of a group. Every other pod
// (DaemonSets, the proxy, system pods) is the platform's and takes room away from the labs.
func IsLabPod(p *corev1.Pod) bool {
	if p.Labels[names.LabelLab] != "" {
		return true
	}
	c := p.Labels[names.LabelComponent]
	if c == "" {
		c = p.Labels["app"]
	}
	return c == names.ComponentVPN || c == names.ComponentGateway
}

// FreeForLabs is the room labs can take on every schedulable node, in the order of nodes: the allocatable less the platform
// reserve (the absolute Node part, then Percent of what is left) and the requests of the pods that are not lab pods (finished
// pods hold nothing), less the packing reserve, which is taken per node from what is left. The operator and the agent both use it, so they report the same room.
func FreeForLabs(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve, packing PackingReserve) []NodeFree {
	requested := map[string]Amount{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || IsLabPod(p) {
			continue
		}
		r, sum := PodRequests(p), requested[p.Spec.NodeName]
		requested[p.Spec.NodeName] = Amount{sum.CPU + r.CPU, sum.Memory + r.Memory}
	}
	var out []NodeFree
	for i := range nodes {
		n := &nodes[i]
		if !Schedulable(n, selector, tolerations) {
			continue
		}
		req := requested[n.Name]
		cpu := max(n.Status.Allocatable.Cpu().MilliValue()-reserve.Node.CPU, 0)
		mem := max(n.Status.Allocatable.Memory().Value()-reserve.Node.Memory, 0)
		cpu -= cpu * int64(reserve.Percent) / 100
		mem -= mem * int64(reserve.Percent) / 100
		cpu, mem = max(cpu-req.CPU, 0), max(mem-req.Memory, 0)
		cpu, mem = packing.Apply(cpu, mem)
		out = append(out, NodeFree{Name: n.Name, CPU: cpu, Memory: mem})
	}
	return out
}
