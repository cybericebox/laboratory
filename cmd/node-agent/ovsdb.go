//go:build linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
)

// portKey computes a stable OVS port name (max 15 chars) for a device interface.
func portKey(namespace, connection, iface string) string {
	h := sha256.Sum256([]byte(namespace + "/" + connection + "/" + iface))
	return fmt.Sprintf("p%x", h[:4])
}

// genevePortName computes a stable OVS Geneve port name for a remote node address.
func genevePortName(remoteAddr string) string {
	h := sha256.Sum256([]byte(remoteAddr))
	return fmt.Sprintf("gv%x", h[:4])
}

// labIfaceName computes the OVS internal port name for a lab's VPN/gateway access iface.
// Stays within Linux IFNAMSIZ (15 usable chars).
func labIfaceName(labName string) string {
	full := "lab-" + labName
	if len(full) <= 15 {
		return full
	}
	h := sha256.Sum256([]byte(labName))
	return fmt.Sprintf("lb-%x", h[:4]) // 11 chars
}

// OVSManager programs the single br-ovs bridge via ovs-vsctl exec.
type OVSManager struct {
	bridge string
}

func newOVSManager(bridge string) (*OVSManager, error) {
	m := &OVSManager{bridge: bridge}
	return m, m.ensureBridge()
}

func (m *OVSManager) vsctl(args ...string) error {
	out, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ovs-vsctl %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *OVSManager) ensureBridge() error {
	return m.vsctl("--may-exist", "add-br", m.bridge)
}

// AddInternalPort creates an OVS internal port in br-ovs (idempotent).
func (m *OVSManager) AddInternalPort(name string) error {
	return m.vsctl(
		"--may-exist", "add-port", m.bridge, name,
		"--", "set", "Interface", name, "type=internal",
	)
}

// AddGenevePort creates a Geneve tunnel port (idempotent). key=flow means per-flow tun_id.
func (m *OVSManager) AddGenevePort(name, remoteIP string) error {
	return m.vsctl(
		"--may-exist", "add-port", m.bridge, name,
		"--", "set", "Interface", name,
		"type=geneve",
		fmt.Sprintf("options:remote_ip=%s", remoteIP),
		"options:key=flow",
	)
}

// DelPort removes a port from br-ovs (idempotent).
func (m *OVSManager) DelPort(name string) error {
	return m.vsctl("--if-exists", "del-port", m.bridge, name)
}

// PortExists checks whether a named port exists on br-ovs.
func (m *OVSManager) PortExists(name string) (bool, error) {
	out, err := exec.Command("ovs-vsctl", "list-ports", m.bridge).Output()
	if err != nil {
		return false, fmt.Errorf("list-ports: %w", err)
	}
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p == name {
			return true, nil
		}
	}
	return false, nil
}
