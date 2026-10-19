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

// GuaranteedResources returns the one amount a device container gets for both
// its requests and its limits, so the pod is Guaranteed and the scheduler sees
// the real load. Per resource the limit wins, then the request, then the
// default. A quantity that does not parse, is zero or negative (it would remove the limit) or is beyond the absolute
// bound is skipped, and one above the chart maximum is clamped to it, so an object that predates the validation
// cannot run a pod without limits. Nil when nothing is set.
func GuaranteedResources(spec *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) corev1.ResourceList {
	var cpuReq, cpuLim, memReq, memLim string
	if spec != nil {
		cpuReq, cpuLim, memReq, memLim = spec.CPURequest, spec.CPULimit, spec.MemoryRequest, spec.MemoryLimit
	}
	var out corev1.ResourceList
	set := func(name corev1.ResourceName, maxQ string, candidates ...string) {
		isCPU := name == corev1.ResourceCPU
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
			if out == nil {
				out = corev1.ResourceList{}
			}
			out[name] = q
			return
		}
	}
	set(corev1.ResourceCPU, defaults.MaxCPU, cpuLim, cpuReq, defaults.CPU)
	set(corev1.ResourceMemory, defaults.MaxMemory, memLim, memReq, defaults.Memory)
	return out
}

// LabNeed is what the pods of a lab's container devices request together, as the scheduler counts them (GuaranteedResources
// per device; switches and hubs run no pod).
func LabNeed(spec *laboratoryv1alpha1.LabSpec, defaults DeviceDefaults) (cpu, mem int64) {
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue
		}
		if r := GuaranteedResources(d.Resources, defaults); r != nil {
			cpu, mem = cpu+r.Cpu().MilliValue(), mem+r.Memory().Value()
		}
	}
	return cpu, mem
}
