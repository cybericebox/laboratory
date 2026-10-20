package limits

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// DeviceDefaults are the CPU and memory of a device container that declares
// none. Empty strings leave the resource unset.
type DeviceDefaults struct {
	CPU    string
	Memory string
	// MaxCPU and MaxMemory are the chart maxima of one device (limits.device.max*): a larger value of an existing
	// object is clamped to them. Empty or "0" = no maximum beyond the absolute bound of the limits package.
	MaxCPU    string
	MaxMemory string
}

// pick is the first usable candidate: a quantity that parses, is positive and within the absolute bound, clamped to the
// chart maximum maxQ. False when none is usable.
func pick(isCPU bool, maxQ string, candidates ...string) (resource.Quantity, bool) {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := DeviceQuantity(c, isCPU); err != nil {
			continue
		}
		q, err := resource.ParseQuantity(c)
		if err != nil {
			continue
		}
		if _, err := DeviceQuantity(maxQ, isCPU); err == nil {
			if m, _ := resource.ParseQuantity(maxQ); q.Cmp(m) > 0 {
				q = m
			}
		}
		return q, true
	}
	return resource.Quantity{}, false
}

// DevicePodResources returns the requests and the limits of a device container. Memory is one amount for both (limit,
// else request, else the default). CPU may burst: the request is cpuRequest (else cpuLimit, else the default) and the
// limit is cpuLimit (else the request), never below the request. A quantity that does not parse, is zero or negative
// (it would remove the limit) or is beyond the absolute bound is skipped, and one above the chart maximum is clamped to
// it, so an object that predates the validation cannot run a pod without limits. Both nil when nothing is set.
func DevicePodResources(spec *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) (requests, limits corev1.ResourceList) {
	var cpuReq, cpuLim, memReq, memLim string
	if spec != nil {
		cpuReq, cpuLim, memReq, memLim = spec.CPURequest, spec.CPULimit, spec.MemoryRequest, spec.MemoryLimit
	}
	add := func(name corev1.ResourceName, req, lim resource.Quantity) {
		if requests == nil {
			requests, limits = corev1.ResourceList{}, corev1.ResourceList{}
		}
		requests[name], limits[name] = req, lim
	}
	if req, ok := pick(true, defaults.MaxCPU, cpuReq, cpuLim, defaults.CPU); ok {
		lim, ok := pick(true, defaults.MaxCPU, cpuLim)
		if !ok || lim.Cmp(req) < 0 {
			lim = req.DeepCopy()
		}
		add(corev1.ResourceCPU, req, lim)
	}
	if mem, ok := pick(false, defaults.MaxMemory, memLim, memReq, defaults.Memory); ok {
		add(corev1.ResourceMemory, mem, mem.DeepCopy())
	}
	return requests, limits
}

// DeviceRequests is what the scheduler, the quotas and the reservations count for a device container: the requests of
// DevicePodResources. The CPU limit above it is a burst that nothing plans with. Nil when nothing is set.
func DeviceRequests(spec *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) corev1.ResourceList {
	requests, _ := DevicePodResources(spec, defaults)
	return requests
}

// LabNeed is what the pods of a lab's container devices request together, as the scheduler counts them (DeviceRequests
// per device; switches and hubs run no pod).
func LabNeed(spec *laboratoryv1alpha1.LabSpec, defaults DeviceDefaults) (cpu, mem int64) {
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue
		}
		if r := DeviceRequests(d.Resources, defaults); r != nil {
			cpu, mem = cpu+r.Cpu().MilliValue(), mem+r.Memory().Value()
		}
	}
	return cpu, mem
}
