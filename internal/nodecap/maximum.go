package nodecap

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// LabelNodeGroup marks a lab node of an autoscaled node group (infra sets it when the node joins); its value is the group. The
// budget of the static maximum covers these nodes only: a node without it (a fixed node such as the controller) counts with
// its real room alone.
const LabelNodeGroup = "cybericebox.com/node-group"

// VirtualRoomName is the name of the virtual room in a placement: no Node can have it (a slash is not allowed in a Node name).
const VirtualRoomName = "virtual/budget"

// Maximum is the static maximum capacity of the lab nodes (chart capacity.budget): Budget is what the autoscaled nodes may give
// together at most, as infra renders it: the number of nodes times (allocatable less the DaemonSets' requests), no percentage.
// Zero: no maximum, the maximum is the current capacity.
type Maximum struct {
	Budget Amount
}

// Set says a budget is configured.
func (m Maximum) Set() bool { return m.Budget != (Amount{}) }

// ParseMaximum reads the budget: both quantities empty is none; otherwise both are required and positive.
func ParseMaximum(cpu, mem string) (Maximum, error) {
	if cpu == "" && mem == "" {
		return Maximum{}, nil
	}
	var m Maximum
	for _, c := range []struct {
		env, val string
		cpu      bool
	}{{"CAPACITY_BUDGET_CPU", cpu, true}, {"CAPACITY_BUDGET_MEMORY", mem, false}} {
		if c.val == "" {
			return Maximum{}, fmt.Errorf("%s is required with a capacity budget (both cpu and memory, or neither)", c.env)
		}
		q, err := resource.ParseQuantity(c.val)
		if err != nil || q.Sign() <= 0 {
			return Maximum{}, fmt.Errorf("%s %q is not a positive quantity", c.env, c.val)
		}
		if c.cpu {
			m.Budget.CPU = q.MilliValue()
		} else {
			m.Budget.Memory = q.Value()
		}
	}
	return m, nil
}

// Virtual is the room for labs that the budget still allows beyond the autoscaled nodes that exist now (Remaining), less the
// platform reserve percent and the packing reserve percent, as the operator keeps them on a real node. The absolute per-node
// reserves are not taken: the number of nodes still to come is not known. One aggregate amount: a gang may span nodes, and the
// cluster autoscaler picks the nodes.
func (m Maximum) Virtual(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve, packing PackingReserve) Amount {
	left := m.Remaining(nodes, pods, selector, tolerations)
	cpu, mem := left.CPU, left.Memory
	cpu -= cpu * int64(reserve.Percent) / 100
	mem -= mem * int64(reserve.Percent) / 100
	cpu, mem = PackingReserve{Percent: packing.Percent}.Apply(cpu, mem)
	return Amount{CPU: cpu, Memory: mem}
}

// Remaining is the budget less what the autoscaled nodes that exist give, counted the way infra counts the budget: for every lab
// node of the pool with LabelNodeGroup (cordoned, NotReady, booting and the ones an autoscaler removes too: the autoscaler counts
// them in its totals) its allocatable less the requests of its DaemonSet pods; per resource, never below zero, before any
// reserve of the operator. It is also what a percentage tenant quota counts on top of the allocatable of the nodes that exist.
func (m Maximum) Remaining(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration) Amount {
	if !m.Set() {
		return Amount{}
	}
	grouped := map[string]bool{}
	var have Amount
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[LabelNodeGroup] == "" || !InPool(withoutAutoscalerTaints(n), selector, tolerations) {
			continue
		}
		grouped[n.Name] = true
		have.CPU += n.Status.Allocatable.Cpu().MilliValue()
		have.Memory += n.Status.Allocatable.Memory().Value()
	}
	for i := range pods {
		p := &pods[i]
		if !grouped[p.Spec.NodeName] || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || !ownedByDaemonSet(p) {
			continue
		}
		r := PodRequests(p)
		have.CPU -= r.CPU
		have.Memory -= r.Memory
	}
	return Amount{CPU: max(m.Budget.CPU-have.CPU, 0), Memory: max(m.Budget.Memory-have.Memory, 0)}
}

// withoutAutoscalerTaints is the node without the taints a cluster autoscaler puts on a node it removes.
func withoutAutoscalerTaints(n *corev1.Node) *corev1.Node {
	out := n.DeepCopy()
	out.Spec.Taints = nil
	for _, t := range n.Spec.Taints {
		if t.Key != "ToBeDeletedByClusterAutoscaler" && t.Key != "DeletionCandidateOfClusterAutoscaler" {
			out.Spec.Taints = append(out.Spec.Taints, t)
		}
	}
	return out
}

// ownedByDaemonSet says a DaemonSet runs the pod.
func ownedByDaemonSet(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}
