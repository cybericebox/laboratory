package chart_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// consoleCables.enabled is off by default and reaches the operator and the agent through the shared environment.
func TestConsoleCablesSwitchReachesTheComponents(t *testing.T) {
	if env := operatorConfig(t); env["CONSOLE_CABLES_ENABLED"] != "false" {
		t.Errorf("default CONSOLE_CABLES_ENABLED = %q, want false", env["CONSOLE_CABLES_ENABLED"])
	}
	if env := operatorConfig(t, "--set", "consoleCables.enabled=true"); env["CONSOLE_CABLES_ENABLED"] != "true" {
		t.Errorf("CONSOLE_CABLES_ENABLED = %q, want true", env["CONSOLE_CABLES_ENABLED"])
	}
}

// The lines are moved by the device access service, so consoleCables.enabled renders it even with the console and the checks off; with
// all three off it is not rendered.
func TestConsoleCablesRenderTheDeviceAccessService(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "consoleCables.enabled=true", "-s", "templates/deviceaccess/daemonset.yaml")...)
	if err != nil || !strings.Contains(out, "kind: DaemonSet") {
		t.Fatalf("consoleCables.enabled must render the device access DaemonSet: %v\n%s", err, out)
	}
	if out, err := helmTemplate(t, append(append([]string{}, agentSet...), "-s", "templates/deviceaccess/daemonset.yaml")...); err == nil && strings.Contains(out, "kind: DaemonSet") {
		t.Fatal("with every user off the service is not rendered")
	}
}

func withConsolePort(s *laboratoryv1alpha1.LabSpec) {
	s.Devices = append(s.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "frr",
		ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}})
}

func withConsoleCable(s *laboratoryv1alpha1.LabSpec) {
	s.ConsoleCables = append(s.ConsoleCables, laboratoryv1alpha1.ConsoleCableTemplate{
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}})
}

// With consoleCables.enabled off the API server refuses a lab with a port or a cable (CONSOLE_CABLES_UNAVAILABLE); with it on they pass.
func TestFeatureAdmissionConsoleCables(t *testing.T) {
	ctx := context.Background()
	f := startFeatureCluster(t, nil)
	ns := f.group(t, "cc-off", nil, nil)
	refused(t, f.other.Create(ctx, lab(ns, "port", withConsolePort)), "CONSOLE_CABLES_UNAVAILABLE")
	refused(t, f.other.Create(ctx, lab(ns, "cable", withConsoleCable)), "CONSOLE_CABLES_UNAVAILABLE")
	accepted(t, f.other.Create(ctx, lab(ns, "plain", nil)))
	// a Device or a ConsoleCable made directly (not through a Lab) is refused too
	dev := func(name string, ports bool) *laboratoryv1alpha1.Device {
		d := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{LabRef: "l", Name: name, Type: laboratoryv1alpha1.DeviceTypeContainer}}
		if ports {
			d.Spec.SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
		}
		return d
	}
	refused(t, f.other.Create(ctx, dev("pc1", true)), "CONSOLE_CABLES_UNAVAILABLE")
	accepted(t, f.other.Create(ctx, dev("pc2", false)))
	cc := &laboratoryv1alpha1.ConsoleCable{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.ConsoleCableSpec{LabRef: "l",
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}
	refused(t, f.other.Create(ctx, cc), "CONSOLE_CABLES_UNAVAILABLE")

	on := startFeatureCluster(t, nil, "--set", "consoleCables.enabled=true")
	ns = on.group(t, "cc-on", nil, nil)
	accepted(t, on.other.Create(ctx, lab(ns, "port", withConsolePort)))
	accepted(t, on.other.Create(ctx, lab(ns, "cable", withConsoleCable)))
	accepted(t, on.other.Create(ctx, &laboratoryv1alpha1.ConsoleCable{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.ConsoleCableSpec{LabRef: "l",
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}))
}
