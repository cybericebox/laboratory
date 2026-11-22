package switches

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A lab with a console or serial port, or with a console cable, needs the consoleCables switch.
func TestConsoleCablesNeedTheirSwitch(t *testing.T) {
	off := Everything()
	off.ConsoleCables = false
	withPort := &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer,
		ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}}}}
	withCable := &laboratoryv1alpha1.LabSpec{ConsoleCables: []laboratoryv1alpha1.ConsoleCableTemplate{{
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}}
	for name, spec := range map[string]*laboratoryv1alpha1.LabSpec{"port": withPort, "cable": withCable} {
		r := off.Refuse(spec)
		if r == nil || r.Reason != ReasonConsoleCablesUnavailable {
			t.Fatalf("%s: got %v, want %s", name, r, ReasonConsoleCablesUnavailable)
		}
		if r := Everything().Refuse(spec); r != nil {
			t.Fatalf("%s: refused with the switch on: %v", name, r)
		}
	}
	if r := off.Refuse(&laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "a"}}}); r != nil {
		t.Fatalf("a lab without ports is not affected: %v", r)
	}
}
