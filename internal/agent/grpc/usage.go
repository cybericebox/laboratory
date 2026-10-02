package grpc

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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
	u := foldUsage(list.Items)[ns]
	if u == nil {
		u = map[usageKey]deviceUsage{}
	}
	return u
}

// foldUsage sums the containers of each pod metric into the usage of its device, by namespace and (lab, device).
func foldUsage(items []metricsv1beta1.PodMetrics) map[string]map[usageKey]deviceUsage {
	out := map[string]map[usageKey]deviceUsage{}
	for i := range items {
		pm := &items[i]
		lab := pm.Labels[names.LabelLab]
		device := pm.Labels[names.LabelDevice]
		if lab == "" || device == "" {
			continue
		}
		// A device runs exactly one pod (Deployment, replicas=1). During a brief
		// recreation overlap two PodMetrics may share the (lab, device) labels —
		// take the last one rather than summing, so usage reflects one pod, not two.
		var u deviceUsage
		for c := range pm.Containers {
			usage := pm.Containers[c].Usage
			u.cpuMillicores += usage.Cpu().MilliValue()
			u.memoryBytes += usage.Memory().Value()
		}
		if out[pm.Namespace] == nil {
			out[pm.Namespace] = map[usageKey]deviceUsage{}
		}
		out[pm.Namespace][usageKey{lab: lab, device: device}] = u
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

// namespaceDeviceScheduling returns the scheduler state of the Devices of a namespace,
// keyed by (lab CR name, device name). Best effort: nil when the list fails.
func (h *Handler) namespaceDeviceScheduling(ctx context.Context, ns string) map[usageKey]*laboratoryv1alpha1.PodSchedule {
	list, err := h.cs.LaboratoryV1alpha1().Devices(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	out := make(map[usageKey]*laboratoryv1alpha1.PodSchedule, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		out[usageKey{lab: d.Spec.LabRef, device: d.Spec.Name}] = d.Status.Scheduling
	}
	return out
}
