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
	if err := r.ensureVPNDeployment(ctx, "ns", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayDeployment(ctx, "ns", false, nil); err != nil {
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

// The pods of labs use the bin-packing scheduler profile when one is configured; with none they keep the default scheduler.
func TestPodsCarryTheLabSchedulerName(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"laboratory-binpack", ""} {
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		r := &LabGroupReconciler{Client: c, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "lab:v1", GatewayImage: "lab:v1", SchedulerName: name}
		if err := r.ensureVPNDeployment(ctx, "ns", false, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureGatewayDeployment(ctx, "ns", false, nil); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"vpn", "gateway"} {
			var d appsv1.Deployment
			if err := c.Get(ctx, types.NamespacedName{Name: n, Namespace: "ns"}, &d); err != nil {
				t.Fatal(err)
			}
			if got := d.Spec.Template.Spec.SchedulerName; got != name {
				t.Errorf("%s: scheduler %q, want %q", n, got, name)
			}
		}
		device := &laboratoryv1alpha1.Device{}
		device.Spec.Name, device.Spec.LabRef = "web", "lab"
		_, _, _, spec := (&DeviceReconciler{SchedulerName: name}).workloadTemplate(device, false)
		if spec.SchedulerName != name {
			t.Errorf("device: scheduler %q, want %q", spec.SchedulerName, name)
		}
	}
}

func TestDeployPriorityIsAnIntegerAnnotation(t *testing.T) {
	obj := func(v string) *laboratoryv1alpha1.Lab {
		l := &laboratoryv1alpha1.Lab{}
		if v != "" {
			l.Annotations = map[string]string{"laboratory.cybericebox.com/deploy-priority": v}
		}
		return l
	}
	for in, want := range map[string]int{"": 0, "5": 5, " -2 ": -2, "high": 0} {
		if got := deployPriority(obj(in)); got != want {
			t.Errorf("%q = %d, want %d", in, got, want)
		}
	}
}
