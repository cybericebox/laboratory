package names

import (
	"crypto/sha256"
	"fmt"
)

// LabIfaceNameByIndex returns the in-netns interface name for lab N inside VPN
// and Gateway pods (e.g. lab1 … lab254). Safe to share between the two pod
// types because each pod has its own network namespace.
// It is also the host-side OVS port name for the VPN leg only — the gateway
// leg uses GWIfaceNameByIndex, since both host-side ports live in the root
// netns and would otherwise collide when a lab has VPN and internet enabled.
func LabIfaceNameByIndex(n uint) string {
	return fmt.Sprintf("lab%d", n)
}

// GWIfaceNameByIndex returns the host-side OVS port name for lab N's internet
// (gateway) leg (e.g. gw1 … gw254). The pod-side interface inside the gateway
// pod is still named LabIfaceNameByIndex(n).
func GWIfaceNameByIndex(n uint) string {
	return fmt.Sprintf("gw%d", n)
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
