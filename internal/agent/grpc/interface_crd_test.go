package grpc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The CRD itself refuses a bad interface name or MAC, for a client that bypasses the agent.
func TestLabCRDValidatesInterfaces(t *testing.T) {
	h, k8s := newTestHandler(t)
	readyGroup(t, h, k8s, "team-iface", "team-iface", nil)
	labs := h.cs.LaboratoryV1alpha1().Labs("team-iface")
	try := func(name string, iface laboratoryv1alpha1.InterfaceSpec) error {
		iface.Addr.Type = laboratoryv1alpha1.AddrType("dhcp")
		lab := &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-iface"},
			Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{
				Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx",
				Interfaces: []laboratoryv1alpha1.InterfaceSpec{iface},
			}}},
		}
		_, err := labs.Create(context.Background(), lab, metav1.CreateOptions{})
		return err
	}
	for i, bad := range []laboratoryv1alpha1.InterfaceSpec{
		{Name: "a@x,eth0@y|ff:ff:ff:ff:ff:ff"}, {Name: "lo"}, {Name: "accessport"},
		{Name: "Eth1"}, {Name: "averyveryverylongname"},
		{Name: "eth1", MAC: "ff:ff:ff:ff:ff:ff"}, {Name: "eth1", MAC: "01:00:5e:00:00:01"},
		{Name: "eth1", MAC: "00:00:00:00:00:00"}, {Name: "eth1", MAC: "nonsense"},
	} {
		if err := try("bad"+string(rune('a'+i)), bad); err == nil {
			t.Errorf("%+v must be rejected by the CRD", bad)
		}
	}
	for i, ok := range []laboratoryv1alpha1.InterfaceSpec{
		{Name: "eth0"}, {Name: "eth1"}, {Name: "eth1", MAC: "random"}, {Name: "eth1", MAC: "02:aa:bb:cc:dd:ee"},
	} {
		if err := try("ok"+string(rune('a'+i)), ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}
