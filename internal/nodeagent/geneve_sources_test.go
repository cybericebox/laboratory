//go:build linux

package nodeagent

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNodeAddressesAreTheInternalIPv4Ones(t *testing.T) {
	node := func(addrs ...corev1.NodeAddress) corev1.Node {
		return corev1.Node{Status: corev1.NodeStatus{Addresses: addrs}}
	}
	got := NodeAddresses([]corev1.Node{
		node(corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.105.3"}, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.9"}),
		node(corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.105.2"}, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::2"}),
		node(corev1.NodeAddress{Type: corev1.NodeHostName, Address: "worker"}),
	})
	if len(got) != 2 || !got[0].Equal(net.ParseIP("192.168.105.2")) || !got[1].Equal(net.ParseIP("192.168.105.3")) {
		t.Fatalf("%v: only the internal IPv4 addresses, sorted (an external address is not a VTEP)", got)
	}
}
