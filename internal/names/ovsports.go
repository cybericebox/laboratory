package names

import (
	"crypto/sha256"
	"fmt"
)

// LabIfaceNameByIndex returns the in-netns interface name for lab N inside VPN
// and Gateway pods (e.g. lab1 … lab254). Safe to share between the two pod
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

// DevicePortKey computes a stable OVS port name (≤15 chars) for a device interface.
// Keyed by namespace + pod name + interface name — stable across reconciles and
// independent of the Connection CRD name (which may not exist yet at pod creation).
// 48 bits of the hash keep the veth peer name ("v" prefix, 14 chars) within
// IFNAMSIZ while making birthday collisions between ports negligible.
func DevicePortKey(namespace, podName, ifaceName string) string {
	h := sha256.Sum256([]byte(namespace + "/" + podName + "/" + ifaceName))
	return fmt.Sprintf("p%x", h[:6]) // 13 chars
}
