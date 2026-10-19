package nodecap

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/klog/v2"
)

// Maximum is the static maximum capacity of the lab nodes (chart capacity.*): the most lab nodes the cluster may have, the ones
// that exist included, and the allocatable of one node that may still be added. Nodes 0: the maximum is the current capacity.
type Maximum struct {
	Nodes int
	Added Amount
}

// ParseMaximum reads the static maximum: nodes (0: none) and, when it is set, the allocatable CPU and memory of an added node
// (Kubernetes quantities, both required and positive).
func ParseMaximum(nodes int, cpu, mem string) (Maximum, error) {
	if nodes < 0 {
		return Maximum{}, fmt.Errorf("CAPACITY_MAX_NODES must not be negative (0: the maximum is the current capacity)")
	}
	if nodes == 0 {
		return Maximum{}, nil
	}
	m := Maximum{Nodes: nodes}
	for _, c := range []struct {
		env, val string
		cpu      bool
	}{{"CAPACITY_ADDED_NODE_CPU", cpu, true}, {"CAPACITY_ADDED_NODE_MEMORY", mem, false}} {
		if c.val == "" {
			return Maximum{}, fmt.Errorf("%s is required when CAPACITY_MAX_NODES is set", c.env)
		}
		q, err := resource.ParseQuantity(c.val)
		if err != nil || q.Sign() <= 0 {
			return Maximum{}, fmt.Errorf("%s %q is not a positive quantity", c.env, c.val)
		}
		if c.cpu {
			m.Added.CPU = q.MilliValue()
		} else {
			m.Added.Memory = q.Value()
		}
	}
	return m, nil
}

// virtualNode is the node that may still be added as the DaemonSets see it: the lab node labels (the selector, which holds the
// node-agent-ready label too, and a Linux node) and the taints the lab tolerations tolerate (the lab taint).
func virtualNode(selector map[string]string, tolerations []corev1.Toleration) *corev1.Node {
	n := &corev1.Node{}
	n.Labels = map[string]string{corev1.LabelOSStable: "linux"}
	for k, v := range selector {
		n.Labels[k] = v
	}
	for _, t := range tolerations {
		// a toleration of a node state (not ready, pressure...) says nothing about the lab taint
		if t.Key == "" || strings.HasPrefix(t.Key, "node.kubernetes.io/") || (t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute) {
			continue
		}
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: t.Key, Value: t.Value, Effect: t.Effect})
	}
	return n
}

// matchesNode says whether a pod with this spec may run on the node: its nodeSelector, its required node affinity (labels; a
// field term never matches a node that does not exist) and its tolerations of the node's NoSchedule and NoExecute taints.
func matchesNode(spec *corev1.PodSpec, node *corev1.Node) bool {
	for k, v := range spec.NodeSelector {
		if node.Labels[k] != v {
			return false
		}
	}
	if a := spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		matched := false
		for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if termMatches(term, node.Labels) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		tolerated := false
		for j := range spec.Tolerations {
			if spec.Tolerations[j].ToleratesTaint(klog.Background(), taint, false) {
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

// termMatches is one node selector term against the labels: every expression holds; a term with field expressions or none
// at all matches nothing (as the scheduler reads it for a node without those fields).
func termMatches(term corev1.NodeSelectorTerm, nodeLabels map[string]string) bool {
	if len(term.MatchFields) > 0 || len(term.MatchExpressions) == 0 {
		return false
	}
	ops := map[corev1.NodeSelectorOperator]selection.Operator{
		corev1.NodeSelectorOpIn: selection.In, corev1.NodeSelectorOpNotIn: selection.NotIn, corev1.NodeSelectorOpExists: selection.Exists,
		corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist, corev1.NodeSelectorOpGt: selection.GreaterThan, corev1.NodeSelectorOpLt: selection.LessThan,
	}
	sel := labels.NewSelector()
	for _, e := range term.MatchExpressions {
		op, ok := ops[e.Operator]
		if !ok {
			return false
		}
		r, err := labels.NewRequirement(e.Key, op, e.Values)
		if err != nil {
			return false
		}
		sel = sel.Add(*r)
	}
	return sel.Matches(labels.Set(nodeLabels))
}

// DaemonSetOverhead is what the DaemonSets would request on a lab node that is added: the sum of the requests of the pods of
// every DaemonSet (not being deleted) whose node selector, required node affinity and tolerations let it run on a node with the
// lab labels and the lab taints.
func DaemonSetOverhead(dss []appsv1.DaemonSet, selector map[string]string, tolerations []corev1.Toleration) Amount {
	node := virtualNode(selector, tolerations)
	var sum Amount
	for i := range dss {
		ds := &dss[i]
		if ds.DeletionTimestamp != nil || !matchesNode(&ds.Spec.Template.Spec, node) {
			continue
		}
		r := PodRequests(&corev1.Pod{Spec: ds.Spec.Template.Spec})
		sum.CPU += r.CPU
		sum.Memory += r.Memory
	}
	return sum
}

// Virtual is the part of the maximum that does not exist now: count, the nodes still to come (the maximum less the schedulable
// lab nodes, never below zero: a booting node, one without its node-agent, a cordoned one, one an autoscaler is removing and a
// NotReady one stay virtual), and room, what labs could take on one of them: the added node's allocatable less the platform
// reserve (the absolute part, then the percent), the requests of the DaemonSets that would land there and the packing reserve,
// as FreeForLabs counts a real node.
func (m Maximum) Virtual(nodes []corev1.Node, dss []appsv1.DaemonSet, selector map[string]string, tolerations []corev1.Toleration, reserve Reserve, packing PackingReserve) (count int, room NodeFree) {
	if m.Nodes <= 0 {
		return 0, NodeFree{}
	}
	schedulable := 0
	for i := range nodes {
		if Schedulable(&nodes[i], selector, tolerations) {
			schedulable++
		}
	}
	count = max(m.Nodes-schedulable, 0)
	cpu := max(m.Added.CPU-reserve.Node.CPU, 0)
	mem := max(m.Added.Memory-reserve.Node.Memory, 0)
	cpu -= cpu * int64(reserve.Percent) / 100
	mem -= mem * int64(reserve.Percent) / 100
	ds := DaemonSetOverhead(dss, selector, tolerations)
	cpu, mem = max(cpu-ds.CPU, 0), max(mem-ds.Memory, 0)
	cpu, mem = packing.Apply(cpu, mem)
	return count, NodeFree{CPU: cpu, Memory: mem}
}

// VirtualRooms is the room of each node still to come, named virtual/0, virtual/1, ...: after the real nodes in a placement, so
// the real room is used first.
func VirtualRooms(count int, room NodeFree) []NodeFree {
	out := make([]NodeFree, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, NodeFree{Name: fmt.Sprintf("%s%d", VirtualPrefix, i), CPU: room.CPU, Memory: room.Memory})
	}
	return out
}

// VirtualPrefix starts the name of a virtual room; no Node can have it (a slash is not allowed in a Node name).
const VirtualPrefix = "virtual/"
