package grpc

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// deviceUsage is live resource consumption for one device, summed across a
// single pod's containers.
type deviceUsage struct {
	cpuMillicores int64
	memoryBytes   int64
}

// usageKey identifies a device within a namespace by its (lab, device) labels.
type usageKey struct {
	lab    string
	device string
}

// namespaceUsage returns live per-device usage for a namespace, keyed by
// (lab, device) read from pod labels. Best-effort: returns nil when metrics
// are unavailable (no metrics client wired, or the metrics API errors — e.g.
// metrics-server not installed), so callers report zero usage instead of
// failing monitoring.
func (h *Handler) namespaceUsage(ctx context.Context, ns string) map[usageKey]deviceUsage {
	if h.metrics == nil {
		return nil
	}
	list, err := h.metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	out := make(map[usageKey]deviceUsage)
	for i := range list.Items {
		pm := &list.Items[i]
		lab := pm.Labels[names.LabelLab]
		device := pm.Labels[names.LabelDevice]
		if lab == "" || device == "" {
			continue
		}
		key := usageKey{lab: lab, device: device}
		// A device runs exactly one pod (Deployment, replicas=1). During a brief
		// recreation overlap two PodMetrics may share the (lab, device) labels —
		// take the last one rather than summing, so usage reflects one pod, not two.
		var u deviceUsage
		for c := range pm.Containers {
			usage := pm.Containers[c].Usage
			u.cpuMillicores += usage.Cpu().MilliValue()
			u.memoryBytes += usage.Memory().Value()
		}
		out[key] = u
	}
	return out
}

// fillLabUsage sets live per-device usage on a proto Lab from a namespace usage
// map. A nil map (metrics unavailable) leaves usage at zero.
func fillLabUsage(lab *protobuf.Lab, usage map[usageKey]deviceUsage) {
	if usage == nil || lab.GetStatus() == nil {
		return
	}
	for _, d := range lab.Status.Devices {
		if u, ok := usage[usageKey{lab: lab.Name, device: d.Name}]; ok {
			d.CpuMillicores = u.cpuMillicores
			d.MemoryBytes = u.memoryBytes
		}
	}
}
