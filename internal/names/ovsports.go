package names

import (
	"crypto/sha256"
	"fmt"
)

// LabIfaceNameByIndex returns the OVS port name and in-netns interface name for
// lab N inside VPN or Gateway pods (e.g. lab1 … lab254). Both pod types use the
// same name because they run in separate network namespaces — no collision.
func LabIfaceNameByIndex(n uint) string {
	return fmt.Sprintf("lab%d", n)
}

// LabVPNObjectName returns the Kubernetes object name for the LabVPN belonging to lab.
func LabVPNObjectName(labName string) string { return "labvpn-" + labName }

// LabGatewayObjectName returns the Kubernetes object name for the LabGateway belonging to lab.
func LabGatewayObjectName(labName string) string { return "labgw-" + labName }

// DevicePortKey computes a stable OVS port name (≤15 chars) for a device interface.
// Keyed by namespace + pod name + interface name — stable across reconciles and
// independent of the Connection CRD name (which may not exist yet at pod creation).
func DevicePortKey(namespace, podName, ifaceName string) string {
	h := sha256.Sum256([]byte(namespace + "/" + podName + "/" + ifaceName))
	return fmt.Sprintf("p%x", h[:4]) // 9 chars
}
