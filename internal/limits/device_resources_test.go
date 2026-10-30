package limits

import (
	"testing"

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

// A device keeps exactly the CPU request and limit its author set; with only one of them the limit equals the request. There is no burst
// above the limit and no way to switch the limit off.
func TestCPULimitIsAlwaysSet(t *testing.T) {
	spec := &laboratoryv1alpha1.DeviceResources{CPURequest: "50m", MemoryLimit: "128Mi"}
	req, lim := DevicePodResources(spec, DeviceDefaults{CPU: "100m", Memory: "256Mi"})
	if req.Cpu().MilliValue() != 50 || lim.Cpu().MilliValue() != 50 {
		t.Errorf("limit = request when only one is given: %v / %v", req, lim)
	}
	req, lim = DevicePodResources(nil, DeviceDefaults{CPU: "100m", Memory: "256Mi"})
	if req.Cpu().MilliValue() != 100 || lim.Cpu().MilliValue() != 100 {
		t.Errorf("default profile: %v / %v", req, lim)
	}
	spec.CPULimit = "400m"
	if _, lim = DevicePodResources(spec, DeviceDefaults{MaxCPU: "300m"}); lim.Cpu().MilliValue() != 300 {
		t.Errorf("a declared limit is kept, clamped to the maximum: %v", lim)
	}
}

// A microVM device costs its own requests plus the RuntimeClass overhead wherever the platform sums a lab's devices (the reservation
// check of the agent uses LabNeed); a container device and an overhead-less configuration add nothing.
func TestLabNeedAddsTheMicroVMOverhead(t *testing.T) {
	defaults := DeviceDefaults{CPU: "100m", Memory: "256Mi", MicroVMOverheadCPU: "50m", MicroVMOverheadMemory: "120Mi"}
	spec := &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "a", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "b", Type: laboratoryv1alpha1.DeviceTypeContainer, Profile: laboratoryv1alpha1.DeviceProfileContainer},
		{Name: "c", Type: laboratoryv1alpha1.DeviceTypeContainer, Profile: laboratoryv1alpha1.DeviceProfileMicroVM},
	}}
	cpu, mem := LabNeed(spec, defaults)
	if cpu != 3*100+50 || mem != 3*(256<<20)+120<<20 {
		t.Fatalf("need = %dm %d", cpu, mem)
	}
	cpu, mem = LabNeed(spec, DeviceDefaults{CPU: "100m", Memory: "256Mi"})
	if cpu != 300 || mem != 3*(256<<20) {
		t.Fatalf("no overhead configured: %dm %d", cpu, mem)
	}
}
