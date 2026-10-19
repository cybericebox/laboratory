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
