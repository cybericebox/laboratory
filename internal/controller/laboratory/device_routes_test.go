package laboratory

import (
	"encoding/json"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestNetConfigStaticRoute(t *testing.T) {
	device := &laboratoryv1alpha1.Device{}
	device.Spec.Interfaces = []laboratoryv1alpha1.InterfaceSpec{{
		Name: "eth0",
		Addr: laboratoryv1alpha1.AddrSpec{
			Type: laboratoryv1alpha1.AddrTypeStatic,
			IP:   "10.0.0.2/24",
			Routes: []laboratoryv1alpha1.Route{{
				Dst: "10.1.0.0/16",
				Via: "10.0.0.1",
			}},
		},
	}}
	container := (&DeviceReconciler{NetConfigImage: "netconfig"}).netConfigInitContainer(device)
	if container == nil {
		t.Fatal("missing netconfig container")
	}
	var config []netconfigIface
	if err := json.Unmarshal([]byte(container.Env[0].Value), &config); err != nil {
		t.Fatal(err)
	}
	if len(config) != 1 || len(config[0].Routes) != 1 || config[0].Routes[0].Dst != "10.1.0.0/16" || config[0].Routes[0].Via != "10.0.0.1" {
		t.Fatalf("route lost: %+v", config)
	}
}
