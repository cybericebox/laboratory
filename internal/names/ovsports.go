package names

import (
	"crypto/sha256"
	"fmt"
)

// MaxLabNetworkIndex is the highest number of a gateway leg (interface lab<N>): the one lab address pool is at most a /8, 65535 numbers
// after number 0, which belongs to the VPN clients. lab65535 is 8 characters, far under the 15 of a Linux interface name.
const MaxLabNetworkIndex = 65535

// LabIfaceNameByIndex returns the in-netns interface name for lab N inside VPN
// and Gateway pods (e.g. lab1 … lab32767). Safe to share between the two pod
// types because each pod has its own network namespace. It is never a
// host-side name: see VPNHostPortKey / GWHostPortKey.
func LabIfaceNameByIndex(n uint) string {
	return fmt.Sprintf("lab%d", n)
}

// VPNHostPortKey returns the host-side OVS port name of lab N's VPN leg in a
// group namespace. Lab indexes are allocated per group, so every group has a
// lab 1: the name must include the namespace or two groups' VPN pods on one
// node would fight over the same port. ≤15 chars like DevicePortKey.
func VPNHostPortKey(namespace string, n uint) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/vpn/%d", namespace, n)))
	return fmt.Sprintf("n%x", h[:6])
}

// GWHostPortKey returns the host-side OVS port name of lab N's internet
// (gateway) leg in a group namespace; distinct from the VPN leg of the same lab.
func GWHostPortKey(namespace string, n uint) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/gw/%d", namespace, n)))
	return fmt.Sprintf("g%x", h[:6])
}

// LabVPNObjectName returns the Kubernetes object name for the LabVPN belonging to lab.
func LabVPNObjectName(labName string) string { return "labvpn-" + labName }

// LabGatewayObjectName returns the Kubernetes object name for the LabGateway belonging to lab.
func LabGatewayObjectName(labName string) string { return "labgw-" + labName }

// LabVPNGatewayObjectName is the name of the LabVPN of the VPN gateway named device in the lab; empty device = the gateway of the shorthand
// spec.vpn, whose object keeps LabVPNObjectName. A lab name is a DNS label and never contains ".", so "<lab>.<device>" is never the name
// of another lab's object.
func LabVPNGatewayObjectName(labName, device string) string {
	if device == "" {
		return LabVPNObjectName(labName)
	}
	return LabVPNObjectName(labName) + "." + device
}

// LabInternetGatewayObjectName is LabVPNGatewayObjectName for the LabGateway of an Internet gateway.
func LabInternetGatewayObjectName(labName, device string) string {
	if device == "" {
		return LabGatewayObjectName(labName)
	}
	return LabGatewayObjectName(labName) + "." + device
}

// DevicePortKey computes a stable OVS port name (≤15 chars) for a device interface.
// Keyed by namespace + pod name + interface name — stable across reconciles and
// independent of the Connection CRD name (which may not exist yet at pod creation).
// 48 bits of the hash keep the veth peer name ("v" prefix, 14 chars) within
// IFNAMSIZ while making birthday collisions between ports negligible.
func DevicePortKey(namespace, podName, ifaceName string) string {
	h := sha256.Sum256([]byte(namespace + "/" + podName + "/" + ifaceName))
	return fmt.Sprintf("p%x", h[:6]) // 13 chars
}
