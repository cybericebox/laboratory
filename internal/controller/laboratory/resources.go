package laboratory

import (
	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
)

// DeviceDefaults are the CPU and memory of a device container that declares none, and the chart maxima (see
// limits.DeviceDefaults).
type DeviceDefaults = limits.DeviceDefaults

// guaranteedResources is the one amount a device container gets for both its requests and its limits (see
// limits.GuaranteedResources): the agent counts a lab against its reservation with the same function.
func guaranteedResources(spec *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) corev1.ResourceList {
	return limits.GuaranteedResources(spec, defaults)
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
