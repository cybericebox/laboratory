package laboratory

import (
	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// isBandwidthGateway says a pod is the gateway of a group (the label of the operator, or the old app label of the pods made before it).
func isBandwidthGateway(p *corev1.Pod) bool {
	return p.Labels[names.LabelComponent] == names.ComponentGateway || p.Labels["app"] == names.ComponentGateway && p.Labels[names.LabelComponent] == ""
}

// bandwidthPlan is what the operator writes.
type bandwidthPlanResult struct {
	// gateways is the annotation each gateway pod should carry, by pod.
	gateways map[types.NamespacedName]string
	// nodes is what each node's pods read from the ConfigMap, by node name: the entry limits of the WireGuard demux and the L7 proxy, and the
	// Geneve ceilings the node-agent applies.
	nodes map[string]string
}

// bandwidthPlan works the plan out.
//
// A node's classes come from its label and the reserves of the pods on it (bandwidth.Config.ForNode). The entry class is divided
// between the two entry pods of the node, half each (a participant can use the VPN and the web at once); each throttles its
// participants by it (fair share, up to the participant ceiling). The internet class is divided between all the labs that have a gateway
// on the node, and each gateway applies that share to each of its labs.
func bandwidthPlan(cfg bandwidth.Config, nodes []corev1.Node, pods []corev1.Pod, gatewayLabs map[string]int) bandwidthPlanResult {
	byNode := map[string][]corev1.Pod{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed && p.DeletionTimestamp.IsZero() {
			byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], *p)
		}
	}
	plan := bandwidthPlanResult{gateways: map[types.NamespacedName]string{}, nodes: map[string]string{}}
	// Every demux and L7 proxy of a lab node may forward to a VPN pod on one node: the entry class of a sender is the share of the
	// smallest receiver (cfg.EntryClassKbps).
	senders, minEntry := 0, int64(0)
	for i := range nodes {
		if nodes[i].Labels[names.LabelNodeAgentReady] != "true" {
			continue
		}
		senders++
		if e := cfg.ForNode(&nodes[i], byNode[nodes[i].Name]).EntryKbps; minEntry == 0 || e < minEntry {
			minEntry = e
		}
	}
	for i := range nodes {
		node := &nodes[i]
		onNode := byNode[node.Name]
		limits := cfg.ForNode(node, onNode)
		geneve := cfg.NodeGeneveLimits(limits)
		entry := limits.EntryKbps / 2
		if senders > 0 {
			entry = cfg.EntryClassKbps(minEntry, senders)
		}
		plan.nodes[node.Name] = bandwidth.Limits{ClassKbps: entry, CapKbps: cfg.ParticipantCapMbit * 1000, GeneveKbps: geneve.GeneveKbps, LabCapKbps: geneve.LabCapKbps}.Encode()
		labs := 0
		for j := range onNode {
			if isBandwidthGateway(&onNode[j]) {
				labs += gatewayLabs[onNode[j].Namespace]
			}
		}
		inet := bandwidth.Limits{CapKbps: cfg.LabInternetKbps(limits, labs)}.Encode()
		for j := range onNode {
			if p := &onNode[j]; isBandwidthGateway(p) {
				plan.gateways[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = inet
			}
		}
	}
	return plan
}
