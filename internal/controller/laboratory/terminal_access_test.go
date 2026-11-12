package laboratory

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devices"
)

// A device that enables the terminal gets an access entry of protocol "terminal" with no port. Its url is the console host of the device,
// <device>-<code>--term under the lab domain: the web host label plus the mark of the kind, from the code of the Device. It is empty
// while the device has no code or the lab domain is not set, and a device without a terminal gets no entry.
func TestTerminalAccessEntries(t *testing.T) {
	s := pruneScheme(t)
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab1", Namespace: "team-alpha"}, Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Access: []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessTerminal}}},
		{Name: "new", Type: laboratoryv1alpha1.DeviceTypeContainer, Access: []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessTerminal}}},
		{Name: "off", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}}}
	dev := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: devices.Name("lab1", "web"), Namespace: "team-alpha"}}
	dev.Spec.Name, dev.Spec.LabRef, dev.Spec.Code = "web", "lab1", "k7x"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(dev).Build()
	for _, c2 := range []struct{ domain, url string }{{"", ""}, {"labs.example.com", "https://web-k7x--term.labs.example.com"}} {
		r := &LabReconciler{Client: c, Scheme: s, BaseDomain: c2.domain}
		got := r.buildAccessEntries(context.Background(), lab)
		want := []laboratoryv1alpha1.AccessEntry{{Device: "web", Protocol: "terminal", URL: c2.url}, {Device: "new", Protocol: "terminal"}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("domain %q: %+v", c2.domain, got)
		}
	}
}
