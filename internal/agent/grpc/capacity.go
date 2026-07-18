package grpc

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// GetCapacity reports per-node allocatable resources and how much is already
// requested by scheduled pods, plus cluster totals. A caller divides the
// remaining headroom by a lab's summed device requests to estimate how many
// more labs fit.
func (h *Handler) GetCapacity(ctx context.Context, _ *protobuf.Empty) (*protobuf.CapacityResponse, error) {
	nodes, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := h.k8s.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return computeCapacity(nodes.Items, pods.Items), nil
}

// computeCapacity is the pure aggregation: allocatable per node from node
// status, requested per node from the container requests of every non-terminated
// pod scheduled on it. InitContainers are not counted (a small under-estimate on
// init-heavy pods, immaterial for lab-fit planning).
func computeCapacity(nodes []corev1.Node, pods []corev1.Pod) *protobuf.CapacityResponse {
	reqCPU := make(map[string]int64)
	reqMem := make(map[string]int64)
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for c := range p.Spec.Containers {
			req := p.Spec.Containers[c].Resources.Requests
			reqCPU[p.Spec.NodeName] += req.Cpu().MilliValue()
			reqMem[p.Spec.NodeName] += req.Memory().Value()
		}
	}

	resp := &protobuf.CapacityResponse{}
	for i := range nodes {
		n := &nodes[i]
		alloc := n.Status.Allocatable
		nc := &protobuf.NodeCapacity{
			Name:                     n.Name,
			AllocatableCpuMillicores: alloc.Cpu().MilliValue(),
			AllocatableMemoryBytes:   alloc.Memory().Value(),
			RequestedCpuMillicores:   reqCPU[n.Name],
			RequestedMemoryBytes:     reqMem[n.Name],
		}
		resp.Nodes = append(resp.Nodes, nc)
		resp.AllocatableCpuMillicores += nc.AllocatableCpuMillicores
		resp.AllocatableMemoryBytes += nc.AllocatableMemoryBytes
		resp.RequestedCpuMillicores += nc.RequestedCpuMillicores
		resp.RequestedMemoryBytes += nc.RequestedMemoryBytes
	}
	return resp
}
