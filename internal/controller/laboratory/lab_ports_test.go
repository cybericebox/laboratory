package laboratory

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func portLab(ports []string) *laboratoryv1alpha1.Lab {
	lab := &laboratoryv1alpha1.Lab{}
	lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{
		Name: "sw", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch,
	})
	for i, port := range ports {
		host := fmt.Sprintf("host-%d", i)
		lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{
			Name: host, Type: laboratoryv1alpha1.DeviceTypeContainer,
			Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}},
		})
		lab.Spec.Connections = append(lab.Spec.Connections, laboratoryv1alpha1.ConnectionTemplate{
			Endpoints: []laboratoryv1alpha1.EndpointSpec{
				{Device: "sw", Interface: port}, {Device: host, Interface: "eth0"},
			},
		})
	}
	return lab
}

func TestForwardingPortValidation(t *testing.T) {
	for _, tc := range []struct {
		name, port string
		wantError  bool
	}{
		{"first", "GigabitEthernet0/1", false},
		{"last", "GigabitEthernet0/48", false},
		{"empty", "", true},
		{"zero", "GigabitEthernet0/0", true},
		{"overflow", "GigabitEthernet0/49", true},
		{"lowercase", "gigabitEthernet0/1", true},
		{"leading zero", "GigabitEthernet0/01", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&LabReconciler{}).validateGraph(portLab([]string{tc.port}))
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "sw") {
					t.Fatalf("want port error naming sw, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid port rejected: %v", err)
			}
		})
	}
}

func TestForwardingPortOccupancy(t *testing.T) {
	ports := make([]string, 48)
	for i := range ports {
		ports[i] = fmt.Sprintf("GigabitEthernet0/%d", i+1)
	}
	if err := (&LabReconciler{}).validateGraph(portLab(ports)); err != nil {
		t.Fatalf("48 ports must be available: %v", err)
	}
	if err := (&LabReconciler{}).validateGraph(portLab(append(ports, ports[0]))); err == nil || !strings.Contains(err.Error(), "sw") {
		t.Fatalf("reused 49th port should fail, got %v", err)
	}
	lab := portLab([]string{"GigabitEthernet0/1"})
	lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "sw2", Type: laboratoryv1alpha1.DeviceTypeHub})
	lab.Spec.Connections = append(lab.Spec.Connections, laboratoryv1alpha1.ConnectionTemplate{
		Endpoints: []laboratoryv1alpha1.EndpointSpec{
			{Device: "sw2", Interface: "GigabitEthernet0/1"}, {Device: "sw", Interface: "GigabitEthernet0/2"},
		},
	})
	if err := (&LabReconciler{}).validateGraph(lab); err != nil {
		t.Fatalf("different devices may each use port 1: %v", err)
	}
}

func TestGatewayPortValidation(t *testing.T) {
	for _, gateway := range []string{"vpn", "internet"} {
		for _, tc := range []struct {
			name, port string
			valid      bool
		}{
			{"empty", "", false},
			{"named port", "eth0", true},
			{"other interface", "eth1", false},
			{"switch port", "GigabitEthernet0/1", false},
		} {
			t.Run(gateway+"/"+tc.name, func(t *testing.T) {
				lab := &laboratoryv1alpha1.Lab{}
				lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{
					Name: "host", Type: laboratoryv1alpha1.DeviceTypeContainer,
					Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}},
				}}
				lab.Spec.Connections = []laboratoryv1alpha1.ConnectionTemplate{{Endpoints: []laboratoryv1alpha1.EndpointSpec{
					{Device: gateway, Interface: tc.port}, {Device: "host", Interface: "eth0"},
				}}}
				if gateway == "vpn" {
					lab.Spec.VPN.Enabled = true
				} else {
					lab.Spec.Internet.Enabled = true
				}
				err := (&LabReconciler{}).validateGraph(lab)
				if tc.valid && err != nil {
					t.Fatalf("valid singleton port rejected: %v", err)
				}
				if !tc.valid && (err == nil || !strings.Contains(err.Error(), "InvalidGatewayPort")) {
					t.Fatalf("invalid singleton port %q accepted: %v", tc.port, err)
				}
			})
		}
	}
}

func TestGatewayPortCannotBeReusedAcrossConnections(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{}
	lab.Spec.VPN.Enabled = true
	for _, host := range []string{"host-a", "host-b"} {
		lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{
			Name: host, Type: laboratoryv1alpha1.DeviceTypeContainer,
			Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}},
		})
		lab.Spec.Connections = append(lab.Spec.Connections, laboratoryv1alpha1.ConnectionTemplate{Endpoints: []laboratoryv1alpha1.EndpointSpec{
			{Device: "vpn", Interface: "eth0"}, {Device: host, Interface: "eth0"},
		}})
	}
	err := (&LabReconciler{}).validateGraph(lab)
	if err == nil || !strings.Contains(err.Error(), "DuplicateGatewayPort") {
		t.Fatalf("reused VPN port must fail before materialization: %v", err)
	}
}

func TestConnectionNameWithForwardingPort(t *testing.T) {
	a := []laboratoryv1alpha1.EndpointSpec{{Device: "sw", Interface: "GigabitEthernet0/1"}, {Device: "host", Interface: "eth0"}}
	b := []laboratoryv1alpha1.EndpointSpec{a[1], a[0]}
	name := connectionName("lab", a)
	if issues := validation.IsDNS1123Subdomain(name); len(issues) != 0 {
		t.Fatalf("invalid Kubernetes name %q: %v", name, issues)
	}
	if name != connectionName("lab", b) {
		t.Fatal("endpoint order changed connection name")
	}
	c := []laboratoryv1alpha1.EndpointSpec{{Device: "sw", Interface: "GigabitEthernet0-1"}, a[1]}
	if name == connectionName("lab", c) {
		t.Fatal("slash port collided with hyphen port")
	}
}

func TestInvalidDeviceNameFailsValidation(t *testing.T) {
	for _, name := range []string{strings.Repeat("d", 36), "Web", "web_1", "-web"} {
		lab := &laboratoryv1alpha1.Lab{}
		lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{
			Name: name, Type: laboratoryv1alpha1.DeviceTypeContainer,
		})
		err := (&LabReconciler{}).validateGraph(lab)
		if err == nil || !strings.Contains(err.Error(), "InvalidDeviceName") {
			t.Errorf("%q: want InvalidDeviceName, got %v", name, err)
		}
	}
}
