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

const accessChain = "CYBERICEBOX_VPN_ACCESS"

func NewIPTablesManager(wgIface string) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	return &IPTablesManager{ipt: ipt, wgIface: wgIface}, nil
}

// SetupForwardPolicy sets FORWARD policy to DROP and installs a default-deny
// access chain for client-to-lab traffic. Called once on pod start before the
// reconcile loop begins.
func (m *IPTablesManager) SetupForwardPolicy() error {
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP: %w", err)
	}
	rules := []string{
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		fmt.Sprintf("-i %s -o lab+ -j %s", m.wgIface, accessChain),
		fmt.Sprintf("-i %s -o %s -j DROP", m.wgIface, m.wgIface),
	}
	for _, r := range rules {
		if err := m.ipt.AppendUnique("filter", "FORWARD", splitArgs(r)...); err != nil {
			return fmt.Errorf("iptables FORWARD %s: %w", r, err)
		}
	}
	return m.ReplaceAccessRules(nil)
}

// ReplaceAccessRules atomically in intent replaces all client-to-lab accepts.
// The chain is flushed before new accepts are appended, so any transient state
// is deny-only. The caller supplies one rule per permitted client/lab CIDR.
func (m *IPTablesManager) ReplaceAccessRules(rules []AccessRule) error {
	if err := m.ipt.ClearChain("filter", accessChain); err != nil {
		return fmt.Errorf("clear access chain: %w", err)
	}
	for _, rule := range rules {
		if err := m.ipt.AppendUnique("filter", accessChain,
			"-s", rule.SourceCIDR,
			"-d", rule.DestinationCIDR,
			"-j", "ACCEPT",
		); err != nil {
			return fmt.Errorf("allow %s to %s: %w", rule.SourceCIDR, rule.DestinationCIDR, err)
		}
	}
	return nil
}

func (m *IPTablesManager) Cleanup() {
	rules := []string{
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		fmt.Sprintf("-i %s -o lab+ -j %s", m.wgIface, accessChain),
		fmt.Sprintf("-i %s -o %s -j DROP", m.wgIface, m.wgIface),
	}
	for _, r := range rules {
		_ = m.ipt.Delete("filter", "FORWARD", splitArgs(r)...)
	}
	_ = m.ipt.ClearAndDeleteChain("filter", accessChain)
}
