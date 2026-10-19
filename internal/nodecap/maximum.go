package nodecap

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// LabelNodeGroup marks a lab node of an autoscaled node group (infra sets it when the node joins); its value is the group. The
// budget of the static maximum covers these nodes only: a node without it (a fixed node such as the controller) counts with
// its real room alone.
const LabelNodeGroup = "cybericebox.io/node-group"

// VirtualRoomName is the name of the virtual room in a placement: no Node can have it (a slash is not allowed in a Node name).
const VirtualRoomName = "virtual/budget"

// Maximum is the static maximum capacity of the lab nodes (chart capacity.budget): Budget is the room for labs the autoscaled
// nodes may give together at most (the totals infra gives the cluster autoscaler, less the control plane, the platform pods
// and the DaemonSets). Zero: no maximum, the maximum is the current capacity.
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

// Virtual is the room for labs that the budget still allows beyond the autoscaled nodes that exist now: the budget less the
// room of the schedulable lab nodes with LabelNodeGroup (their allocatable less the requests of the pods that are not lab pods,
// the same terms as the budget), per resource and never below zero, then less the platform reserve percent and the packing
// reserve percent, as the operator keeps them on a real node. The absolute per-node reserves are not taken: the number of nodes
// still to come is not known. A booting, cordoned, NotReady node or one an autoscaler removes is not schedulable, so its room
// stays virtual. One aggregate amount: a gang may span nodes, and the cluster autoscaler picks the nodes.
func (m Maximum) Virtual(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve, packing PackingReserve) Amount {
	left := m.Remaining(nodes, pods, selector, tolerations)
	cpu, mem := left.CPU, left.Memory
	cpu -= cpu * int64(reserve.Percent) / 100
	mem -= mem * int64(reserve.Percent) / 100
	cpu, mem = PackingReserve{Percent: packing.Percent}.Apply(cpu, mem)
	return Amount{CPU: cpu, Memory: mem}
}

// Remaining is the budget less the room of the schedulable autoscaled nodes (their allocatable less the requests of the pods
// that are not lab pods), per resource, never below zero, before any reserve of the operator: what a percentage tenant quota
// counts on top of the allocatable of the nodes that exist.
func (m Maximum) Remaining(nodes []corev1.Node, pods []corev1.Pod, selector map[string]string, tolerations []corev1.Toleration) Amount {
	if !m.Set() {
		return Amount{}
	}
	grouped := map[string]bool{}
	for i := range nodes {
		if nodes[i].Labels[LabelNodeGroup] != "" {
			grouped[nodes[i].Name] = true
		}
	}
	var have Amount
	for _, f := range FreeForLabs(nodes, pods, selector, tolerations, Reserve{}, PackingReserve{}) {
		if grouped[f.Name] {
			have.CPU += f.CPU
			have.Memory += f.Memory
		}
	}
	return Amount{CPU: max(m.Budget.CPU-have.CPU, 0), Memory: max(m.Budget.Memory-have.Memory, 0)}
}
