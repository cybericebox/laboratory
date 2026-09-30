package laboratory

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// capacity is the CPU and memory of the nodes lab pods can run on.
type capacity struct {
	nodes       int
	allocatable amount
	free        amount
}

// schedulableNode reports whether lab pods can be placed on the node: it is
// schedulable and Ready, matches the lab node selector and every NoSchedule or
// NoExecute taint is tolerated by the lab tolerations.
func schedulableNode(node *corev1.Node, selector map[string]string, tolerations []corev1.Toleration) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			return false
		}
	}
	for k, v := range selector {
		if node.Labels[k] != v {
			return false
		}
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for j := range tolerations {
			if tolerations[j].ToleratesTaint(klog.Background(), taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// snapshotCapacity sums the allocatable resources of the schedulable nodes and
// subtracts the requests of the pods scheduled on them. Free is summed per node
// and never negative on a node, so an overcommitted node does not hide room
// elsewhere. Finished pods hold no resources.
func snapshotCapacity(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration) capacity {
	requested := map[string]amount{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		requested[p.Spec.NodeName] = requested[p.Spec.NodeName].add(podRequests(p))
	}
	var c capacity
	for i := range nodes {
		n := &nodes[i]
		if !schedulableNode(n, selector, tolerations) {
			continue
		}
		alloc := amount{cpu: n.Status.Allocatable.Cpu().MilliValue(), mem: n.Status.Allocatable.Memory().Value()}
		c.nodes++
		c.allocatable = c.allocatable.add(alloc)
		c.free = c.free.add(alloc.sub(requested[n.Name]).floorZero())
	}
	return c
}

// fit is the verdict of the resource check for one lab.
type fit int

const (
	fitOK fit = iota
	// fitWait: not enough is free now; it may fit once running labs settle.
	fitWait
	// fitNever: the lab exceeds the whole schedulable capacity (minus headroom),
	// so waiting will not help.
	fitNever
	// fitNoNodes: there is no node to place the pods on.
	fitNoNodes
)

// check decides whether a lab needing need can be admitted while headroomPercent
// of the allocatable CPU and memory stays free. A lab that requests nothing
// always fits.
func (c capacity) check(need amount, headroomPercent int) fit {
	if need == (amount{}) {
		return fitOK
	}
	if c.nodes == 0 {
		return fitNoNodes
	}
	reserve := amount{cpu: c.allocatable.cpu * int64(headroomPercent) / 100, mem: c.allocatable.mem * int64(headroomPercent) / 100}
	usable := c.allocatable.sub(reserve)
	if need.cpu > usable.cpu || need.mem > usable.mem {
		return fitNever
	}
	avail := c.free.sub(reserve)
	if need.cpu > avail.cpu || need.mem > avail.mem {
		return fitWait
	}
	return fitOK
}

// take reserves need out of the free capacity after a lab was admitted.
func (c *capacity) take(need amount) {
	c.free = c.free.sub(need).floorZero()
}
