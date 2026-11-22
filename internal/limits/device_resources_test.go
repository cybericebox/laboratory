package limits

import (
	"errors"
	"strings"
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

// A device whose memory is above limits.device.maxMemory is refused (owner decision C4), never clamped: nothing counts a
// clamped size for it (the agent's reservation check skips it, as the scheduler does) and its pod resources are what it asks
// for. A device at the maximum is accepted and counted at its size.
func TestDeviceMemoryAboveMaximumIsNotClamped(t *testing.T) {
	defaults := DeviceDefaults{CPU: "100m", Memory: "256Mi", MaxCPU: "2", MaxMemory: "4Gi"}
	above := &laboratoryv1alpha1.DeviceResources{MemoryLimit: "8Gi"}
	at := &laboratoryv1alpha1.DeviceResources{MemoryLimit: "4Gi"}
	if req, lim := DevicePodResources(above, defaults); req.Memory().Value() != 8<<30 || lim.Memory().Value() != 8<<30 {
		t.Errorf("memory above the maximum was clamped: %v / %v", req.Memory(), lim.Memory())
	}
	if req, _ := DevicePodResources(at, defaults); req.Memory().Value() != 4<<30 {
		t.Errorf("memory at the maximum: %v", req.Memory())
	}
	spec := &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "at", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: at},
		{Name: "above", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: above, Profile: laboratoryv1alpha1.DeviceProfileMicroVM},
	}}
	if cpu, mem := LabNeed(spec, defaults); cpu != 100 || mem != 4<<30 {
		t.Fatalf("a refused device must count nothing: cpu %dm memory %d", cpu, mem)
	}
}

// The refusal names the device, the requested and the maximum memory, with the coded reason; at the maximum, or without
// one, nothing is refused. A request alone, or the default, above the maximum is refused too.
func TestCheckDeviceMemoryRefusesAboveTheMaximum(t *testing.T) {
	defaults := DeviceDefaults{Memory: "256Mi", MaxMemory: "4Gi"}
	err := CheckDeviceMemory("big", &laboratoryv1alpha1.DeviceResources{MemoryLimit: "8Gi"}, defaults)
	var refused *MemoryAboveMaximumError
	if !errors.As(err, &refused) || refused.Reason() != ReasonDeviceMemoryAboveMaximum {
		t.Fatalf("not refused: %v", err)
	}
	for _, want := range []string{"\"big\"", "8Gi", "4Gi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %s", err, want)
		}
	}
	if CheckDeviceMemory("r", &laboratoryv1alpha1.DeviceResources{MemoryRequest: "5Gi"}, defaults) == nil {
		t.Error("a request alone above the maximum must be refused")
	}
	if CheckDeviceMemory("d", nil, DeviceDefaults{Memory: "8Gi", MaxMemory: "4Gi"}) == nil {
		t.Error("a default above the maximum must be refused")
	}
	if err := CheckDeviceMemory("at", &laboratoryv1alpha1.DeviceResources{MemoryLimit: "4Gi"}, defaults); err != nil {
		t.Errorf("at the maximum: %v", err)
	}
	for _, none := range []string{"", "0"} {
		if err := CheckDeviceMemory("big", &laboratoryv1alpha1.DeviceResources{MemoryLimit: "8Gi"}, DeviceDefaults{MaxMemory: none}); err != nil {
			t.Errorf("no maximum %q: %v", none, err)
		}
	}
	spec := &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "hub", Type: laboratoryv1alpha1.DeviceTypeHub},
		{Name: "at", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: &laboratoryv1alpha1.DeviceResources{MemoryLimit: "4Gi"}},
		{Name: "above", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: &laboratoryv1alpha1.DeviceResources{MemoryLimit: "8Gi"}},
	}}
	if err := CheckLabDeviceMemory(spec, defaults); !errors.As(err, &refused) || refused.Device != "above" {
		t.Fatalf("lab: %v", err)
	}
	if err := CheckLabDeviceMemory(&laboratoryv1alpha1.LabSpec{Devices: spec.Devices[:2]}, defaults); err != nil {
		t.Fatalf("lab at the maximum: %v", err)
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
