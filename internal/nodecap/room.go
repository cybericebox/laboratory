package nodecap

import (
	corev1 "k8s.io/api/core/v1"
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

// LargestDevice is the largest device the nodes can hold: per resource the largest Allocatable of any node (the two may
// come from different nodes, so it can slightly overstate a device that needs both at once). Zero for no node.
func LargestDevice(rooms []NodeRoom) Amount {
	var out Amount
	for _, r := range rooms {
		out.CPU, out.Memory = max(out.CPU, r.Allocatable.CPU), max(out.Memory, r.Allocatable.Memory)
	}
	return out
}
