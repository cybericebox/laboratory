package laboratory

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// The owner of a device pod is marked with labels; the environment the participant reads carries no UID.
func TestDevicePodOwnerIsALabelNotEnvironment(t *testing.T) {
	device := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{UID: "device-uid", OwnerReferences: []metav1.OwnerReference{{Kind: ownerKindLab, UID: "lab-uid"}}}}
	device.Spec.Name = "web"
	device.Spec.LabRef = "lab"
	labels, selector, _, spec := (&DeviceReconciler{}).workloadTemplate(device, false)
	if labels[names.LabelLabUID] != "lab-uid" || labels[names.LabelDeviceUID] != "device-uid" {
		t.Errorf("owner labels = %v", labels)
	}
	if _, ok := selector[names.LabelLabUID]; ok {
		t.Errorf("owner label leaked into the selector: %v", selector)
	}
	for _, c := range spec.Containers {
		for _, e := range c.Env {
			if e.Name == "LIFECYCLE_LAB_UID" || e.Name == "LIFECYCLE_DEVICE_UID" {
				t.Errorf("container %s still carries %s", c.Name, e.Name)
			}
		}
	}
}
