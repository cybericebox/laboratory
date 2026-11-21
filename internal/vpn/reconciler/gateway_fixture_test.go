//go:build linux

package reconciler

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// vpnLab makes a ready lab with one VPN gateway named "vpn" that has the /24 cidr and the leg number index, and the LabVPN object of that
// gateway.
func vpnLab(namespace, name, cidr string, index uint) (*lab.Lab, *lab.LabVPN) {
	l := &lab.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       lab.LabSpec{Gateways: []lab.GatewayTemplate{{Name: "vpn", Kind: lab.GatewayKindVPN}}},
		Status: lab.LabStatus{Phase: lab.PhaseReady, Gateways: []lab.GatewayStatus{
			{Name: "vpn", Kind: lab.GatewayKindVPN, CIDR: cidr, NetworkIndex: index, Ready: true},
		}},
	}
	leg := &lab.LabVPN{
		ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName(name, "vpn"), Namespace: namespace},
		Spec:       lab.LabVPNSpec{LabName: name, Device: "vpn", NetworkIndex: index},
	}
	return l, leg
}
