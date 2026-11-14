package grpc

import (
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A spec that wires a device with internet: deny to an Internet gateway never gets past the agent.
func TestAgentRefusesInternetDeniedDeviceOnTheInternetNetwork(t *testing.T) {
	ep := func(d, i string) laboratoryv1alpha1.EndpointSpec {
		return laboratoryv1alpha1.EndpointSpec{Device: d, Interface: i}
	}
	spec := &laboratoryv1alpha1.LabSpec{
		Gateways:    []laboratoryv1alpha1.GatewayTemplate{{Name: "out", Kind: laboratoryv1alpha1.GatewayKindInternet}},
		Devices:     []laboratoryv1alpha1.DeviceTemplate{{Name: "bot", Type: laboratoryv1alpha1.DeviceTypeContainer, Internet: laboratoryv1alpha1.InternetDeny}},
		Connections: []laboratoryv1alpha1.ConnectionTemplate{{Endpoints: []laboratoryv1alpha1.EndpointSpec{ep("out", "eth0"), ep("bot", "eth0")}}},
	}
	if err := validateDeviceExtras(spec); err == nil || !strings.Contains(err.Error(), "internet: deny") {
		t.Fatalf("%v", err)
	}
	spec.Devices[0].Internet = "sometimes"
	spec.Connections = nil
	if err := validateDeviceExtras(spec); err == nil {
		t.Fatal("an unknown internet value is refused")
	}
}
