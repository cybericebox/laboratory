package laboratory

import (
	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// bandwidthKind says which of the pods that carry a bandwidth ceiling a pod is.
type bandwidthKind int

const (
	notBandwidthPod bandwidthKind = iota
	bandwidthGateway
	bandwidthEntry // the WireGuard demux and the L7 proxy
)

// bandwidthKindOf classifies a pod. Gateways are the group's own pods (the label of the operator, or the old app label of the pods
// made before it); the entry pods are the two proxies by their app label.
func bandwidthKindOf(p *corev1.Pod) bandwidthKind {
	switch {
	case p.Labels[names.LabelComponent] == names.ComponentGateway, p.Labels["app"] == names.ComponentGateway && p.Labels[names.LabelComponent] == "":
		return bandwidthGateway
	case p.Labels["app"] == names.ProxyL7App, p.Labels["app"] == names.WGDemuxApp:
		return bandwidthEntry
	}
	return notBandwidthPod
}

// bandwidthPlan is the annotation each pod should carry, by pod.
//
// A node's classes come from its label and the reserves of the pods on it (bandwidth.Config.ForNode). The entry class is divided
// between the two entry pods of the node, half each (a participant can use the VPN and the web at once); each throttles its
// participants by it (fair share, up to the participant ceiling). The internet class is divided between all the labs that have a gateway
// on the node, and each gateway applies that share to each of its labs.
func bandwidthPlan(cfg bandwidth.Config, nodes []corev1.Node, pods []corev1.Pod, gatewayLabs map[string]int) map[types.NamespacedName]string {
	byNode := map[string][]corev1.Pod{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed && p.DeletionTimestamp.IsZero() {
			byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], *p)
		}
	}
	plan := map[types.NamespacedName]string{}
	for i := range nodes {
		node := &nodes[i]
		onNode := byNode[node.Name]
		if len(onNode) == 0 {
			continue
		}
		limits := cfg.ForNode(node, onNode)
		labs := 0
		for j := range onNode {
			if bandwidthKindOf(&onNode[j]) == bandwidthGateway {
				labs += gatewayLabs[onNode[j].Namespace]
			}
		}
		entry := bandwidth.Limits{ClassKbps: limits.EntryKbps / 2, CapKbps: cfg.ParticipantCapMbit * 1000}
		inet := bandwidth.Limits{CapKbps: cfg.LabInternetKbps(limits, labs)}
		for j := range onNode {
			p := &onNode[j]
			key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
			switch bandwidthKindOf(p) {
			case bandwidthEntry:
				plan[key] = entry.Encode()
			case bandwidthGateway:
				plan[key] = inet.Encode()
			}
		}
	}
	return plan
}
