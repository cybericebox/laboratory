package laboratory

import (
	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
)

// DeviceDefaults are the CPU and memory of a device container that declares none, and the chart maxima (see
// limits.DeviceDefaults).
type DeviceDefaults = limits.DeviceDefaults

// deviceRequests is what a device container counts for the scheduler and the quotas (see limits.DeviceRequests): the agent
// counts a lab against its reservation with the same function.
func deviceRequests(spec *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) corev1.ResourceList {
	return limits.DeviceRequests(spec, defaults)
}

// deviceResources builds the container resource requirements of a device: memory requests equal limits, CPU may burst
// above its request (see limits.DevicePodResources), so the pod is Burstable.
func deviceResources(device *laboratoryv1alpha1.Device, defaults DeviceDefaults) corev1.ResourceRequirements {
	requests, lims := limits.DevicePodResources(device.Spec.Resources, defaults)
	if requests == nil {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: lims}
}
