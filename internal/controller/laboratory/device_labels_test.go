package laboratory

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// The labels a caller put on a Lab reach its Devices and their pods; the platform's own
// keys cannot be overridden by them.
func TestLabelsReachDeviceAndPod(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "ctf", Labels: map[string]string{
		"event": "e1", "app": "spoof", names.LabelDeployGroup: "g",
	}}}
	dl := deviceLabels(lab)
	if dl["event"] != "e1" || dl[names.LabelLab] != "ctf" {
		t.Fatalf("device labels: %v", dl)
	}
	if _, ok := dl[names.LabelDeployGroup]; ok {
		t.Fatal("internal keys stay on the lab")
	}

	device := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "ctf-web", Labels: dl},
		Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: "ctf", Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx"},
	}
	podLabels, selector, _, _ := (&DeviceReconciler{}).workloadTemplate(device, false)
	if podLabels["event"] != "e1" || podLabels["app"] != "web" || podLabels[names.LabelDevice] != "web" {
		t.Fatalf("pod labels: %v", podLabels)
	}
	if len(selector) != 2 {
		t.Fatalf("the selector must stay (lab, device): %v", selector)
	}
}
