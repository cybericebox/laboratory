//go:build linux

package gateway

import (
	"fmt"
	"strings"
	
	"github.com/coreos/go-iptables/iptables"
)

func splitArgs(s string) []string { return strings.Fields(s) }

type IPTablesManager struct {
	ipt      *iptables.IPTables
	extIface string
}

func NewIPTablesManager(extIface string) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	return &IPTablesManager{ipt: ipt, extIface: extIface}, nil
}

// SetupForwardRules sets the FORWARD policy to DROP and installs static rules that
// allow lab interfaces to reach the internet while blocking cross-lab traffic.
// Called once on pod start before the reconcile loop begins.
func (m *IPTablesManager) SetupForwardRules() error {
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP: %w", err)
	}
	rules := [][3]string{
		// lab{N} → internet
		{"filter", "FORWARD", fmt.Sprintf("-o %s -j ACCEPT", m.extIface)},
		// return traffic from internet → lab{N}
		{"filter", "FORWARD", fmt.Sprintf("-i %s -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT", m.extIface)},
	}
	for _, r := range rules {
		args := splitArgs(r[2])
		if err := m.ipt.AppendUnique(r[0], r[1], args...); err != nil {
			return fmt.Errorf("iptables %s %s %s: %w", r[0], r[1], r[2], err)
		}
	}
	return nil
}

func (m *IPTablesManager) AddMasquerade(labCIDR string) error {
	return m.ipt.AppendUnique(
		"nat", "POSTROUTING",
		"-s", labCIDR, "-o", m.extIface, "-j", "MASQUERADE",
	)
}

func (m *IPTablesManager) DelMasquerade(labCIDR string) {
	_ = m.ipt.Delete(
		"nat", "POSTROUTING",
		"-s", labCIDR, "-o", m.extIface, "-j", "MASQUERADE",
	)
}
