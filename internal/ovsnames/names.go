package ovsnames

import (
	"crypto/sha256"
	"fmt"
)

// LabIfaceName computes the OVS internal port name for a lab's VPN access iface.
// Stays within Linux IFNAMSIZ (15 usable chars).
func LabIfaceName(labName string) string {
	full := "lab-" + labName
	if len(full) <= 15 {
		return full
	}
	h := sha256.Sum256([]byte(labName))
	return fmt.Sprintf("lb-%x", h[:4]) // 11 chars
}

// LabGWIfaceName computes the OVS internal port name for a lab's gateway access iface.
// Uses "gw-" prefix to avoid collision with LabIfaceName on the same node.
func LabGWIfaceName(labName string) string {
	full := "gw-" + labName
	if len(full) <= 15 {
		return full
	}
	h := sha256.Sum256([]byte("gw:" + labName))
	return fmt.Sprintf("gw%x", h[:4]) // 10 chars
}

// DevicePortKey computes a stable OVS port name (≤15 chars) for a device interface.
// Keyed by namespace + pod name + interface name — stable across reconciles and
// independent of the Connection CRD name (which may not exist yet at pod creation).
func DevicePortKey(namespace, podName, ifaceName string) string {
	h := sha256.Sum256([]byte(namespace + "/" + podName + "/" + ifaceName))
	return fmt.Sprintf("p%x", h[:4]) // 9 chars
}
