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
