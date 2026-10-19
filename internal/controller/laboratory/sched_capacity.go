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
	nodes int
	// allocatable is the sum over the schedulable nodes less the node reserve: what a percentage tenant quota is a percentage of.
	allocatable amount
	// perNode is the room of every schedulable node (rooms).
	perNode []nodeRoom
	// nodeLimits is, per node of the pool (cordoned, NotReady and rebooting ones too), the most it can give one pod:
	// allocatable less the node reserve, the platform percent and the packing reserve, whatever runs there
	// (nodecap.DeviceLimits). A pod that fits none of them, CPU and memory together, can never be placed.
	nodeLimits []amount
	// maxNode is the per-resource maximum of nodeLimits (nodecap.LargestDevice, the agent's largest device): an upper bound
	// per resource only.
	maxNode amount
	// labUsed is the requests of the lab pods bound to each node (the held pods of stopped labs included).
	labUsed map[string]amount
	// vmax is the number of lab nodes still to come up to the static maximum (nodecap.Maximum.Virtual), virtual the number
	// admission places on in this pass (vmax, or 0 under the breaker) and vroom the room for labs on one of them. All zero
	// without a maximum.
	vmax, virtual int
	vroom         nodeRoom
}

// virtualRooms are n rooms of the nodes still to come, named virtual/0, virtual/1, ...
func (c capacitySnapshot) virtualRooms(n int) []nodeRoom {
	virtual := nodecap.VirtualRooms(n, nodecap.NodeFree{CPU: c.vroom.CPU, Memory: c.vroom.Memory})
	out := make([]nodeRoom, 0, len(virtual))
	for _, v := range virtual {
		out = append(out, nodeRoom{Name: v.Name, CPU: v.CPU, Memory: v.Memory})
	}
	return out
}

// rooms is the room FOR labs on each schedulable node, in the order of the nodes, before the usage of the labs, and after them
// the virtual rooms of the nodes still to come that admission may place on now: the real room is used first.
func (c capacitySnapshot) rooms() []nodeRoom {
	return append(append([]nodeRoom(nil), c.perNode...), c.virtualRooms(c.virtual)...)
}

// emptyRooms is rooms with every node still to come up to the maximum, the breaker or not: what a gang could ever have, for
// the DoesNotFit verdict.
func (c capacitySnapshot) emptyRooms() []nodeRoom {
	return append(append([]nodeRoom(nil), c.perNode...), c.virtualRooms(c.vmax)...)
}

// withMaximum adds the nodes still to come up to the static maximum: count of them with room each; breaker: admission places
// on none of them in this pass (they still count for the verdicts).
func (c *capacitySnapshot) withMaximum(count int, room nodecap.NodeFree, breaker bool) {
	c.vmax, c.vroom = count, nodeRoom{CPU: room.CPU, Memory: room.Memory}
	c.virtual = count
	if breaker {
		c.virtual = 0
	}
}

// freeRooms is rooms less the requests of the lab pods bound to each node, the held pods of stopped labs included (they hold
// resources): where the placement simulation of a pass starts. Lab pods that are dispatched but not on a node yet are placed
// by the plan.
func (c capacitySnapshot) freeRooms() []nodeRoom { return without(c.rooms(), c.labUsed) }

// schedulableNode: see nodecap.Schedulable.
func schedulableNode(node *corev1.Node, selector map[string]string, tolerations []corev1.Toleration) bool {
	return nodecap.Schedulable(node, selector, tolerations)
}

// snapshotCapacity reads the per-node room for labs (rooms: allocatable less the platform reserves, the requests of the pods
// that are not lab pods and the packing reserve), the lab pods bound to each node (labUsed), the per-node limits of the pool
// and the sum of the schedulable nodes' allocatable. Finished pods hold no resources.
func snapshotCapacity(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, nodeReserve amount, platformPercent int, packing nodecap.PackingReserve) capacitySnapshot {
	c := capacitySnapshot{labUsed: map[string]amount{}}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed && nodecap.IsLabPod(p) {
			c.labUsed[p.Spec.NodeName] = c.labUsed[p.Spec.NodeName].add(podRequests(p))
		}
	}
	reserve := nodecap.Reserve{Node: nodecap.Amount{CPU: nodeReserve.cpu, Memory: nodeReserve.mem}, Percent: platformPercent}
	for _, f := range nodecap.FreeForLabs(nodes, pods, selector, tolerations, reserve, packing) {
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
	}
	for _, l := range nodecap.DeviceLimits(nodes, selector, tolerations, reserve, packing) {
		c.nodeLimits = append(c.nodeLimits, amount{cpu: l.CPU, mem: l.Memory})
		c.maxNode = amount{cpu: max(c.maxNode.cpu, l.CPU), mem: max(c.maxNode.mem, l.Memory)}
	}
	return c
}

// fitsOneNode says whether some node of the pool, empty, holds need: CPU and memory on the same node; a node still to come up to
// the maximum counts with its room.
func (c capacitySnapshot) fitsOneNode(need amount) bool {
	for _, l := range c.nodeLimits {
		if need.cpu <= l.cpu && need.mem <= l.mem {
			return true
		}
	}
	return c.vmax > 0 && need.cpu <= c.vroom.CPU && need.mem <= c.vroom.Memory
}
