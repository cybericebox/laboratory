package laboratory

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
}

// guaranteedResources returns the one amount a device container gets for both
// its requests and its limits, so the pod is Guaranteed and the scheduler sees
// the real load. Per resource the limit wins, then the request, then the
// default; unparseable quantities are skipped. Nil when nothing is set.
func guaranteedResources(spec *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) corev1.ResourceList {
	var cpuReq, cpuLim, memReq, memLim string
	if spec != nil {
		cpuReq, cpuLim, memReq, memLim = spec.CPURequest, spec.CPULimit, spec.MemoryRequest, spec.MemoryLimit
	}
	var out corev1.ResourceList
	set := func(name corev1.ResourceName, candidates ...string) {
		for _, c := range candidates {
			if c == "" {
				continue
			}
			q, err := resource.ParseQuantity(c)
			if err != nil {
				continue
			}
			if out == nil {
				out = corev1.ResourceList{}
			}
			out[name] = q
			return
		}
	}
	set(corev1.ResourceCPU, cpuLim, cpuReq, defaults.CPU)
	set(corev1.ResourceMemory, memLim, memReq, defaults.Memory)
	return out
}

// deviceResources builds the container resource requirements of a device:
// requests equal limits (see guaranteedResources).
func deviceResources(device *laboratoryv1alpha1.Device, defaults DeviceDefaults) corev1.ResourceRequirements {
	list := guaranteedResources(device.Spec.Resources, defaults)
	if list == nil {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list.DeepCopy()}
}
