//go:build linux

package vpn

import (
	"fmt"
	"strings"

	"github.com/coreos/go-iptables/iptables"
)

func splitArgs(s string) []string { return strings.Fields(s) }

type IPTablesManager struct {
	ipt     *iptables.IPTables
	wgIface string
}

func NewIPTablesManager(wgIface string) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	return &IPTablesManager{ipt: ipt, wgIface: wgIface}, nil
}

// SetupForwardPolicy sets FORWARD policy to DROP and installs interface-based rules
// that allow WireGuard clients to reach lab segments and vice versa.
// Called once on pod start before the reconcile loop begins.
func (m *IPTablesManager) SetupForwardPolicy() error {
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP: %w", err)
	}
	rules := []string{
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		fmt.Sprintf("-i %s -o lab+ -j ACCEPT", m.wgIface),
		fmt.Sprintf("-i lab+ -o %s -j ACCEPT", m.wgIface),
		fmt.Sprintf("-i %s -o %s -j DROP", m.wgIface, m.wgIface),
	}
	for _, r := range rules {
		if err := m.ipt.AppendUnique("filter", "FORWARD", splitArgs(r)...); err != nil {
			return fmt.Errorf("iptables FORWARD %s: %w", r, err)
		}
	}
	return nil
}

func (m *IPTablesManager) Cleanup() {
	rules := []string{
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		fmt.Sprintf("-i %s -o lab+ -j ACCEPT", m.wgIface),
		fmt.Sprintf("-i lab+ -o %s -j ACCEPT", m.wgIface),
		fmt.Sprintf("-i %s -o %s -j DROP", m.wgIface, m.wgIface),
	}
	for _, r := range rules {
		_ = m.ipt.Delete("filter", "FORWARD", splitArgs(r)...)
	}
}
