package laboratory

import (
	"github.com/cybericebox/laboratory/internal/nodecap"
	corev1 "k8s.io/api/core/v1"
)

// amount is a CPU and memory quantity: millicores and bytes.
type amount struct {
	cpu, mem int64
}

func (a amount) add(b amount) amount { return amount{a.cpu + b.cpu, a.mem + b.mem} }

func (a amount) sub(b amount) amount { return amount{a.cpu - b.cpu, a.mem - b.mem} }

func (a amount) floorZero() amount {
	if a.cpu < 0 {
		a.cpu = 0
	}
	if a.mem < 0 {
		a.mem = 0
	}
	return a
}

// podRequests is the amount the scheduler reserves for a pod: the larger of the
// sum of its containers and its biggest init container, plus the pod overhead.
func podRequests(pod *corev1.Pod) amount {
	r := nodecap.PodRequests(pod)
	return amount{cpu: r.CPU, mem: r.Memory}
}

// nodeRoom is what labs can still take on one node, in millicores and bytes: free after the requests of non-lab pods, the
// platform node reserve and the packing reserve.
type nodeRoom struct {
	Name        string
	CPU, Memory int64
}

// capacitySnapshot is the CPU and memory of the nodes lab pods can run on.
type capacitySnapshot struct {
	nodes       int
	allocatable amount
	free        amount
	// perNode is the room of every schedulable node (rooms).
	perNode []nodeRoom
	// labUsed is the requests of the lab pods bound to each node (the held pods of stopped labs included).
	labUsed map[string]amount
}

// rooms is the room FOR labs on each schedulable node, in the order of the nodes, before the usage of the labs.
func (c capacitySnapshot) rooms() []nodeRoom { return append([]nodeRoom(nil), c.perNode...) }

// freeRooms is rooms less the requests of the lab pods bound to each node, the held pods of stopped labs included (they hold
// resources). Lab pods that are dispatched but not on a node yet are not subtracted.
func (c capacitySnapshot) freeRooms() []nodeRoom {
	out := c.rooms()
	for i := range out {
		used := c.labUsed[out[i].Name]
		out[i].CPU, out[i].Memory = max(out[i].CPU-used.cpu, 0), max(out[i].Memory-used.mem, 0)
	}
	return out
}

// schedulableNode: see nodecap.Schedulable.
func schedulableNode(node *corev1.Node, selector map[string]string, tolerations []corev1.Toleration) bool {
	return nodecap.Schedulable(node, selector, tolerations)
}

// snapshotCapacity sums the allocatable resources of the schedulable nodes (less the absolute platform reserve
// of each) and subtracts the requests of ALL pods scheduled on them (DaemonSets, the proxy and system pods
// included, not only lab pods). Free is summed per node
// and never negative on a node, so an overcommitted node does not hide room
// elsewhere. Finished pods hold no resources.
func snapshotCapacity(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, nodeReserve amount, platformPercent int, packing nodecap.PackingReserve) capacitySnapshot {
	requested := map[string]amount{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		requested[p.Spec.NodeName] = requested[p.Spec.NodeName].add(podRequests(p))
	}
	c := capacitySnapshot{labUsed: map[string]amount{}}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed && nodecap.IsLabPod(p) {
			c.labUsed[p.Spec.NodeName] = c.labUsed[p.Spec.NodeName].add(podRequests(p))
		}
	}
	for _, f := range nodecap.FreeForLabs(nodes, pods, selector, tolerations, nodecap.Reserve{Node: nodecap.Amount{CPU: nodeReserve.cpu, Memory: nodeReserve.mem}, Percent: platformPercent}, packing) {
		c.perNode = append(c.perNode, nodeRoom{Name: f.Name, CPU: f.CPU, Memory: f.Memory})
	}
	for i := range nodes {
		n := &nodes[i]
		if !schedulableNode(n, selector, tolerations) {
			continue
		}
		// What the platform keeps on every node comes off first; the percentage reserve is taken from the rest.
		alloc := amount{cpu: n.Status.Allocatable.Cpu().MilliValue(), mem: n.Status.Allocatable.Memory().Value()}.sub(nodeReserve).floorZero()
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
// of the schedulable CPU and memory (the platform reserve) stays free. A lab that requests nothing
// always fits.
func (c capacitySnapshot) check(need amount, headroomPercent int) fit {
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
func (c *capacitySnapshot) take(need amount) {
	c.free = c.free.sub(need).floorZero()
}
