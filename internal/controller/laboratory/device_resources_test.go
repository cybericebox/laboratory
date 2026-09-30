package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var testDefaults = DeviceDefaults{CPU: "250m", Memory: "256Mi"}

func wantQty(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	got, ok := list[name]
	if !ok {
		t.Fatalf("%s not set, want %s", name, want)
	}
	if got.Cmp(resource.MustParse(want)) != 0 {
		t.Errorf("%s = %s, want %s", name, got.String(), want)
	}
}

// A device without resources gets the defaults, as requests equal to limits.
func TestDeviceResourcesDefaults(t *testing.T) {
	rr := deviceResources(&laboratoryv1alpha1.Device{}, testDefaults)
	for _, list := range []corev1.ResourceList{rr.Requests, rr.Limits} {
		wantQty(t, list, corev1.ResourceCPU, "250m")
		wantQty(t, list, corev1.ResourceMemory, "256Mi")
	}
}

// Empty defaults keep the old behavior for an undeclared device: nothing is set.
func TestDeviceResourcesNoDefaults(t *testing.T) {
	rr := deviceResources(&laboratoryv1alpha1.Device{}, DeviceDefaults{})
	if rr.Requests != nil || rr.Limits != nil {
		t.Errorf("expected empty requirements, got %+v", rr)
	}
}

// Declared requests and limits collapse to the limit, so the pod is Guaranteed.
func TestDeviceResourcesRequestsEqualLimits(t *testing.T) {
	d := &laboratoryv1alpha1.Device{}
	d.Spec.Resources = &laboratoryv1alpha1.DeviceResources{
		CPURequest: "250m", MemoryRequest: "256Mi", CPULimit: "500m", MemoryLimit: "512Mi",
	}
	rr := deviceResources(d, testDefaults)
	for _, list := range []corev1.ResourceList{rr.Requests, rr.Limits} {
		wantQty(t, list, corev1.ResourceCPU, "500m")
		wantQty(t, list, corev1.ResourceMemory, "512Mi")
	}
}

// A request alone becomes the limit too; a missing resource falls back to the
// default; an unparseable quantity is skipped rather than failing the pod.
func TestDeviceResourcesPartialAndInvalid(t *testing.T) {
	d := &laboratoryv1alpha1.Device{}
	d.Spec.Resources = &laboratoryv1alpha1.DeviceResources{
		CPURequest:  "100m",
		MemoryLimit: "not-a-qty",
	}
	rr := deviceResources(d, testDefaults)
	for _, list := range []corev1.ResourceList{rr.Requests, rr.Limits} {
		wantQty(t, list, corev1.ResourceCPU, "100m")
		wantQty(t, list, corev1.ResourceMemory, "256Mi")
	}
}

// Requests and limits must be separate maps, so a later edit of one cannot change the other.
func TestDeviceResourcesMapsAreIndependent(t *testing.T) {
	rr := deviceResources(&laboratoryv1alpha1.Device{}, testDefaults)
	delete(rr.Limits, corev1.ResourceCPU)
	if _, ok := rr.Requests[corev1.ResourceCPU]; !ok {
		t.Error("requests share storage with limits")
	}
}
