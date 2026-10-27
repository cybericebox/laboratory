package grpc

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
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
	// lastRestart is why and when a container of the pod last restarted in place (OOMKilled, Error, ...); "" / zero if never.
	lastRestartReason string
	lastRestartAt     time.Time
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
	all, ok := h.clusterUsage(ctx)
	if !ok {
		return nil
	}
	u := all[ns]
	if u == nil {
		u = map[usageKey]deviceUsage{}
	}
	return u
}

// usageFailureRetry is how soon a failed metrics read is tried again (a slow or failing metrics API must not stall every call).
const usageFailureRetry = 5 * time.Second

// clusterUsage is the usage of all device pods of the cluster, read in one metrics call at most every usageRefresh and shared by
// every caller (a readiness poll of one lab per call used to ask the metrics API once per call). A failed read keeps the last good
// answer for usageFailureRetry; ok is false when there is none.
func (h *Handler) clusterUsage(ctx context.Context) (map[string]map[usageKey]deviceUsage, bool) {
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	if !h.usageAt.IsZero() && time.Since(h.usageAt) < usageRefresh {
		return h.usageAll, h.usageAll != nil
	}
	usage, err := h.fetchUsage(ctx)
	if err != nil {
		h.usageAt = time.Now().Add(usageFailureRetry - usageRefresh)
		return h.usageAll, h.usageAll != nil
	}
	h.usageAll, h.usageAt = usage, time.Now()
	return usage, true
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
		device.LastRestartReason = status.lastRestartReason
		if !status.lastRestartAt.IsZero() {
			device.LastRestartUnixMs = status.lastRestartAt.UnixMilli()
		}
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

// podLastRestart is the newest termination the kubelet recorded for a container that was restarted in place: a device that
// was OOM-killed or crashed runs again (restart count above zero) while its current state looks healthy.
func podLastRestart(pod *corev1.Pod) (string, time.Time) {
	var reason string
	var at time.Time
	statuses := append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		t := status.LastTerminationState.Terminated
		if status.RestartCount == 0 || t == nil {
			continue
		}
		if reason == "" || t.FinishedAt.After(at) {
			reason, at = t.Reason, t.FinishedAt.Time
			if reason == "" {
				reason = "Error"
			}
		}
	}
	return reason, at
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
	items, err := h.readDevices(ctx, ns)
	if err != nil {
		return nil
	}
	out := make(map[usageKey]*laboratoryv1alpha1.PodSchedule, len(items))
	for i := range items {
		d := &items[i]
		out[usageKey{lab: d.Spec.LabRef, device: d.Spec.Name}] = d.Status.Scheduling
	}
	return out
}

// fillGroupPodUsage puts the live usage, the size and the load on the VPN and gateway entries of a group's status (adding an entry
// for a pod the scheduler has no record of). usage is nil when the metrics are not available.
func (h *Handler) fillGroupPodUsage(g *laboratoryv1alpha1.LabGroup, p *protobuf.LabGroup, usage map[string]deviceUsage, clients, internetLabs int) {
	if p.GetStatus() == nil {
		return
	}
	for _, component := range grouppods.PodNames(g) {
		var entry *protobuf.LabGroupPod
		for _, e := range p.Status.Pods {
			if e.Name == component {
				entry = e
			}
		}
		if entry == nil {
			entry = &protobuf.LabGroupPod{Name: component}
			p.Status.Pods = append(p.Status.Pods, entry)
		}
		var res corev1.ResourceRequirements
		units := clients
		if component == names.ComponentVPN {
			res = h.groupPods.VPNFor(g.Spec.VPN.Size)
		} else {
			res = h.groupPods.GatewayFor(g.Spec.Gateway.Size)
			units = internetLabs
		}
		entry.CpuLimitMillicores, entry.MemoryLimitBytes = res.Limits.Cpu().MilliValue(), res.Limits.Memory().Value()
		entry.Units = int32(units)
		if usage != nil {
			entry.UsageAvailable = true
			if u, ok := usage[component]; ok {
				entry.CpuMillicores, entry.MemoryBytes = u.cpuMillicores, u.memoryBytes
			}
		}
	}
}
