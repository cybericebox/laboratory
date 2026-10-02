package laboratory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Every pod the operator creates carries its PriorityClass: the VPN and gateway the group one, a device the device one.
func TestPodsCarryTheirPriorityClass(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	r := &LabGroupReconciler{Client: c, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "lab:v1", GatewayImage: "lab:v1", PriorityClass: "laboratory-group"}
	if err := r.ensureVPNDeployment(ctx, "ns", false); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayDeployment(ctx, "ns", false); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"vpn", "gateway"} {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: n, Namespace: "ns"}, &d); err != nil {
			t.Fatal(err)
		}
		if got := d.Spec.Template.Spec.PriorityClassName; got != "laboratory-group" {
			t.Errorf("%s: class %q, want laboratory-group", n, got)
		}
	}

	device := &laboratoryv1alpha1.Device{}
	device.Spec.Name = "web"
	device.Spec.LabRef = "lab"
	_, _, _, spec := (&DeviceReconciler{PriorityClass: "laboratory-device"}).workloadTemplate(device, false)
	if spec.PriorityClassName != "laboratory-device" {
		t.Errorf("device: class %q, want laboratory-device", spec.PriorityClassName)
	}
}
