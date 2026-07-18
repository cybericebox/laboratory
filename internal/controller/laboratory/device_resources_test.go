package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestDeviceResourcesNil(t *testing.T) {
	rr := deviceResources(&laboratoryv1alpha1.Device{})
	if rr.Requests != nil || rr.Limits != nil {
		t.Errorf("expected empty requirements when Resources unset, got %+v", rr)
	}
}

func TestDeviceResourcesFull(t *testing.T) {
	d := &laboratoryv1alpha1.Device{}
	d.Spec.Resources = &laboratoryv1alpha1.DeviceResources{
		CPURequest:    "250m",
		MemoryRequest: "256Mi",
		CPULimit:      "500m",
		MemoryLimit:   "512Mi",
	}
	rr := deviceResources(d)

	want := map[corev1.ResourceName]struct {
		list corev1.ResourceList
		q    string
	}{
		corev1.ResourceCPU:    {rr.Requests, "250m"},
		corev1.ResourceMemory: {rr.Requests, "256Mi"},
	}
	for name, w := range want {
		got := w.list[name]
		if got.Cmp(resource.MustParse(w.q)) != 0 {
			t.Errorf("request %s = %s, want %s", name, got.String(), w.q)
		}
	}
	if got := rr.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("cpu limit = %s, want 500m", got.String())
	}
	if got := rr.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("mem limit = %s, want 512Mi", got.String())
	}
}

// Empty fields are omitted; unparseable quantities are skipped rather than
// failing the whole pod.
func TestDeviceResourcesPartialAndInvalid(t *testing.T) {
	d := &laboratoryv1alpha1.Device{}
	d.Spec.Resources = &laboratoryv1alpha1.DeviceResources{
		CPURequest:    "100m",       // valid
		MemoryRequest: "",           // omitted
		CPULimit:      "not-a-qty",  // invalid → skipped
	}
	rr := deviceResources(d)
	if _, ok := rr.Requests[corev1.ResourceCPU]; !ok {
		t.Errorf("expected cpu request set")
	}
	if _, ok := rr.Requests[corev1.ResourceMemory]; ok {
		t.Errorf("empty memory request must be omitted")
	}
	if _, ok := rr.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("invalid cpu limit must be skipped")
	}
}
