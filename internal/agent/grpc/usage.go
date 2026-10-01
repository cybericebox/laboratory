package grpc

import (
	"context"

	corev1 "k8s.io/api/core/v1"
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

type devicePodStatus struct {
	phase        string
	reason       string
	restartCount int32
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
// map. A nil map (metrics unavailable) leaves usage at zero. Pods are keyed by the
// CR name of the lab, which differs from lab.Name (the id) for an id that had to be encoded:
// pass it as crName.
func fillLabUsage(lab *protobuf.Lab, usage map[usageKey]deviceUsage, crName ...string) {
	if lab.GetStatus() == nil {
		return
	}
	key := lab.Name
	if len(crName) > 0 {
		key = crName[0]
	}
	for _, d := range lab.Status.Devices {
		d.UsageAvailable = usage != nil
		if usage == nil {
			continue
		}
		if u, ok := usage[usageKey{lab: key, device: d.Name}]; ok {
			d.CpuMillicores = u.cpuMillicores
			d.MemoryBytes = u.memoryBytes
		}
	}
}

func (h *Handler) namespacePodStatus(ctx context.Context, ns string) map[usageKey]devicePodStatus {
	if h.k8s == nil {
		return nil
	}
	pods, err := h.k8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	statuses := make(map[usageKey]devicePodStatus)
	for i := range pods.Items {
		pod := &pods.Items[i]
		key := usageKey{lab: pod.Labels[names.LabelLab], device: pod.Labels[names.LabelDevice]}
		if key.lab == "" || key.device == "" {
			continue
		}
		statuses[key] = devicePodStatus{phase: string(pod.Status.Phase), reason: podReason(pod), restartCount: podRestartCount(pod)}
	}
	return statuses
}

func fillLabPodStatus(lab *protobuf.Lab, pods map[usageKey]devicePodStatus, crName ...string) {
	if pods == nil || lab.GetStatus() == nil {
		return
	}
	key := lab.Name
	if len(crName) > 0 {
		key = crName[0]
	}
	for _, device := range lab.Status.Devices {
		status, found := pods[usageKey{lab: key, device: device.Name}]
		if !found {
			continue
		}
		device.PodPhase = status.phase
		device.PodReason = status.reason
		device.RestartCount = status.restartCount
	}
}

func podReason(pod *corev1.Pod) string {
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil && status.State.Waiting.Reason != "" {
			return status.State.Waiting.Reason
		}
		if status.State.Terminated != nil && status.State.Terminated.Reason != "" {
			return status.State.Terminated.Reason
		}
	}
	return ""
}

func podRestartCount(pod *corev1.Pod) int32 {
	var restarts int32
	for _, status := range pod.Status.InitContainerStatuses {
		restarts += status.RestartCount
	}
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}
	return restarts
}
