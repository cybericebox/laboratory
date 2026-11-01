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
	// noContainer and noMicroVM say the room cannot run a pod of that device profile (set by capacitySnapshot.rooms from the
	// profile policy; the zero value runs everything).
	noContainer, noMicroVM bool
}

// runs says a pod of the profile can be placed in the room.
func (r nodeRoom) runs(profile string) bool {
	if profile == nodecap.ProfileMicroVM {
		return !r.noMicroVM
	}
	return !r.noContainer
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
	// vmax is the room for labs the budget of the static maximum still allows beyond the autoscaled nodes that exist
	// (nodecap.Maximum.Virtual): one aggregate room in rooms, as a gang may span nodes and the cluster autoscaler picks them.
	// Under the breaker the plan keeps new gangs off it; the reservations, the shortfall and the verdicts still count it. Zero
	// without a budget.
	vmax amount
	// profiles maps device profiles to nodes (the zero policy: containers only, anywhere).
	profiles nodecap.ProfilePolicy
	// nodeLabels are the labels of the nodes of perNode, by name (the device profiles they can run).
	nodeLabels map[string]map[string]string
}

// virtualRoom is the aggregate room a the budget still allows, as one room named nodecap.VirtualRoomName; none when a is zero.
func virtualRoom(a amount) []nodeRoom {
	if a == (amount{}) {
		return nil
	}
	return []nodeRoom{{Name: nodecap.VirtualRoomName, CPU: a.cpu, Memory: a.mem}}
}

// rooms is the room FOR labs on each schedulable node, in the order of the nodes, before the usage of the labs, and after them
// the virtual room the budget still allows: the maximum. The real room is used first.
func (c capacitySnapshot) rooms() []nodeRoom {
	out := append([]nodeRoom(nil), c.perNode...)
	if !c.profiles.Active() {
		return append(out, virtualRoom(c.vmax)...)
	}
	for i := range out {
		labels := c.nodeLabels[out[i].Name]
		out[i].noMicroVM = !c.profiles.NodeRuns(labels, nodecap.ProfileMicroVM)
		out[i].noContainer = !c.profiles.NodeRuns(labels, nodecap.ProfileContainer)
	}
	if v := virtualRoom(c.vmax); len(v) > 0 {
		// Nodes the autoscaler adds: a profile runs there when the chart says a node group can add it (microVM: only then; a container
		// when the profile labels are off or the groups are not known).
		v[0].noMicroVM = !c.profiles.CanScaleUp(nodecap.ProfileMicroVM)
		v[0].noContainer = c.profiles.Labels && c.profiles.ScaleUpKnown() && !c.profiles.CanScaleUp(nodecap.ProfileContainer)
		out = append(out, v...)
	}
	return out
}

// emptyRooms is rooms: what a gang could ever have, for the DoesNotFit verdict.
func (c capacitySnapshot) emptyRooms() []nodeRoom { return c.rooms() }

// withMaximum adds the virtual room the budget still allows (room, for the placement) and what the budget adds to the
// allocatable (remaining, before the operator's reserves: the base of a percentage tenant quota is the maximum too).
func (c *capacitySnapshot) withMaximum(room, remaining nodecap.Amount) {
	c.vmax = amount{cpu: room.CPU, mem: room.Memory}
	c.allocatable = c.allocatable.add(amount{cpu: remaining.CPU, mem: remaining.Memory})
}

// usesVirtual says a placement took something of the virtual room of the budget.
func usesVirtual(at map[string]amount) bool { return at[nodecap.VirtualRoomName] != (amount{}) }

// closeVirtual is rooms with the virtual room emptied: what a new gang may be placed on under the breaker.
func closeVirtual(rooms []nodeRoom) []nodeRoom {
	out := append([]nodeRoom(nil), rooms...)
	for i := range out {
		if out[i].Name == nodecap.VirtualRoomName {
			out[i].CPU, out[i].Memory = 0, 0
		}
	}
	return out
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
		if c.nodeLabels == nil {
			c.nodeLabels = map[string]map[string]string{}
		}
		c.nodeLabels[f.Name] = f.Labels
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

// capacityWithMaximum is snapshotCapacity with the virtual room the static maximum still allows (none without a budget).
func capacityWithMaximum(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, nodeReserve amount,
	platformPercent int, packing nodecap.PackingReserve, m nodecap.Maximum) capacitySnapshot {
	c := snapshotCapacity(nodes, pods, selector, tolerations, nodeReserve, platformPercent, packing)
	if m.Set() {
		room := m.Virtual(nodes, pods, selector, tolerations, nodecap.Reserve{Percent: platformPercent}, packing)
		c.withMaximum(room, m.Remaining(nodes, pods, selector, tolerations))
	}
	return c
}

// RoomForLabs is the sum of the rooms a scheduler pass starts from with this configuration (the real nodes, then the virtual
// room of the budget), before any lab runs: the capacity the agent reports must be exactly this (its parity test).
func RoomForLabs(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, reserve nodecap.Reserve,
	packing nodecap.PackingReserve, m nodecap.Maximum) nodecap.Amount {
	c := capacityWithMaximum(nodes, pods, selector, tolerations, amount{cpu: reserve.Node.CPU, mem: reserve.Node.Memory}, reserve.Percent, packing, m)
	var sum nodecap.Amount
	for _, r := range c.rooms() {
		sum.CPU, sum.Memory = sum.CPU+r.CPU, sum.Memory+r.Memory
	}
	return sum
}

// fitsOneNode says whether some node of the pool, empty, holds need: CPU and memory on the same node.
func (c capacitySnapshot) fitsOneNode(need amount) bool {
	for _, l := range c.nodeLimits {
		if need.cpu <= l.cpu && need.mem <= l.mem {
			return true
		}
	}
	return false
}
