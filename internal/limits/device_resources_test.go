package limits

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestDeviceRequestsAreTheRequestsOfThePod(t *testing.T) {
	defaults := DeviceDefaults{CPU: "100m", Memory: "256Mi", MaxCPU: "1"}
	spec := &laboratoryv1alpha1.DeviceResources{CPURequest: "15m", CPULimit: "60m", MemoryRequest: "64Mi", MemoryLimit: "128Mi"}
	req, lim := DevicePodResources(spec, defaults)
	if got := req.Cpu().MilliValue(); got != 15 {
		t.Errorf("request cpu = %d", got)
	}
	if got := lim.Cpu().MilliValue(); got != 60 {
		t.Errorf("limit cpu = %d", got)
	}
	if req.Memory().Value() != 128<<20 || lim.Memory().Value() != 128<<20 {
		t.Errorf("memory %v / %v", req.Memory(), lim.Memory())
	}
	if got := DeviceRequests(spec, defaults); got.Cpu().MilliValue() != 15 {
		t.Errorf("DeviceRequests cpu = %v", got.Cpu())
	}
	if r, l := DevicePodResources(nil, DeviceDefaults{}); r != nil || l != nil {
		t.Errorf("nothing set: %v %v", r, l)
	}
}

// NoCPULimit: a device without a cpuLimit of its own gets a CPU request and no CPU limit (it bursts to the free CPU of the
// node); memory keeps request = limit; a declared cpuLimit is kept; the default off keeps limit = request.
func TestNoCPULimitLeavesTheBurstUncapped(t *testing.T) {
	spec := &laboratoryv1alpha1.DeviceResources{CPURequest: "50m", MemoryLimit: "128Mi"}
	req, lim := DevicePodResources(spec, DeviceDefaults{CPU: "100m", Memory: "256Mi", NoCPULimit: true})
	if req.Cpu().MilliValue() != 50 {
		t.Errorf("request cpu = %v", req.Cpu())
	}
	if _, has := lim[corev1.ResourceCPU]; has {
		t.Errorf("a cpu limit was set: %v", lim)
	}
	if lim.Memory().Value() != 128<<20 || req.Memory().Value() != 128<<20 {
		t.Errorf("memory %v / %v", req.Memory(), lim.Memory())
	}
	// The default profile too.
	req, lim = DevicePodResources(nil, DeviceDefaults{CPU: "100m", Memory: "256Mi", NoCPULimit: true})
	if _, has := lim[corev1.ResourceCPU]; has || req.Cpu().MilliValue() != 100 {
		t.Errorf("default profile: %v / %v", req, lim)
	}
	// An explicit limit stays, clamped to the maximum as before.
	spec.CPULimit = "400m"
	if _, lim = DevicePodResources(spec, DeviceDefaults{NoCPULimit: true, MaxCPU: "300m"}); lim.Cpu().MilliValue() != 300 {
		t.Errorf("declared limit lost: %v", lim)
	}
	// Off: limit = request.
	spec.CPULimit = ""
	if _, lim = DevicePodResources(spec, DeviceDefaults{}); lim.Cpu().MilliValue() != 50 {
		t.Errorf("default must cap at the request: %v", lim)
	}
	// What the scheduler counts is the request either way.
	if got := DeviceRequests(spec, DeviceDefaults{NoCPULimit: true}); got.Cpu().MilliValue() != 50 {
		t.Errorf("DeviceRequests = %v", got.Cpu())
	}
}
