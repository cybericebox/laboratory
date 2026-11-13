package names

import (
	"crypto/sha256"
	"fmt"
	"strings"
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

// LabVPNObjectName is the Kubernetes object name of the LabVPN of the VPN gateway named device in the lab. A lab name is a DNS label and
// never contains ".", so "<lab>.<device>" is never the name of another lab's object (a "-" would be).
func LabVPNObjectName(labName, device string) string { return "labvpn-" + labName + "." + device }

// LabGatewayObjectName is LabVPNObjectName for the LabGateway of an Internet gateway.
func LabGatewayObjectName(labName, device string) string { return "labgw-" + labName + "." + device }

// DHCPPoolName is the name of the DHCP pool of a gateway of a lab: the enable-signal and lease ledger of the gateway's DHCP server.
func DHCPPoolName(labName, device string) string { return "dhcp-" + labName + "." + device + "-0" }

// DHCPPoolGroup is the label value (allocation.cybericebox.com/group) of the DHCP pool of a gateway: the pool name without its "-0". A label
// value is at most 63 characters, and a lab name (up to 63) plus a gateway name (up to 35) is longer than that, so a value that does not fit
// is the hash form (DeployKey: "h" + base36 of SHA-256), never a cut name. Nothing reads the value back to find the gateway: the pool is
// found by DHCPPoolName and owned by its owner reference.
func DHCPPoolGroup(labName, device string) string {
	return DeployKey(strings.TrimSuffix(DHCPPoolName(labName, device), "-0"))
}

// DHCPPoolOwner reads a DHCP pool name made by DHCPPoolName for the lab: the gateway it is for, or "" when the name is not that of a pool
// of this lab.
func DHCPPoolOwner(poolName, labName string) string {
	rest, ok := strings.CutPrefix(poolName, "dhcp-"+labName+".")
	if !ok {
		return ""
	}
	device, ok := strings.CutSuffix(rest, "-0")
	if !ok || device == "" {
		return ""
	}
	return device
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
