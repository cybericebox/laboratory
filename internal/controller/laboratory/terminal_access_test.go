package laboratory

import (
	"context"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A device that enables the terminal gets an access entry of protocol "terminal" with no port and no link; the platform issues
// the link later. The entry does not depend on the lab domain, and a device without a terminal gets none.
func TestTerminalAccessEntries(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Terminal: &laboratoryv1alpha1.DeviceTerminal{Enabled: true}},
		{Name: "off", Type: laboratoryv1alpha1.DeviceTypeContainer, Terminal: &laboratoryv1alpha1.DeviceTerminal{Enabled: false}},
		{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}}}
	for _, domain := range []string{"", "labs.example.com"} {
		r := &LabReconciler{BaseDomain: domain}
		got := r.buildAccessEntries(context.Background(), lab)
		if len(got) != 1 || got[0] != (laboratoryv1alpha1.AccessEntry{Device: "web", Protocol: "terminal"}) {
			t.Fatalf("domain %q: %+v", domain, got)
		}
	}
}
