//go:build linux

package gateway

import (
	"fmt"
	"strings"

	"github.com/coreos/go-iptables/iptables"

	"github.com/cybericebox/laboratory/internal/egress"
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

// egressChain holds the destination filter of the traffic the gateway forwards to the outside.
const egressChain = "LABEGRESS"

// SetupEgressFilter makes the gateway forward lab traffic only to the public internet: in the chain LABEGRESS (jumped
// to first from FORWARD for everything leaving through the external interface) every range of egress.DenyV4 is dropped.
// It is rebuilt from scratch on each start. The gateway's own traffic (its API client) is OUTPUT, not FORWARD, and is not
// touched. The IPv6 equivalents (egress.DenyV6) go the same way when the pod has ip6tables; the gateway forwards no IPv6
// otherwise.
func (m *IPTablesManager) SetupEgressFilter() error {
	if err := setupEgressChain(m.ipt, m.extIface, egress.DenyV4); err != nil {
		return err
	}
	ip6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		return nil // no IPv6 netfilter in this pod: nothing is forwarded over IPv6
	}
	if err := ip6.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return nil
	}
	return setupEgressChain(ip6, m.extIface, egress.DenyV6)
}

func setupEgressChain(ipt *iptables.IPTables, extIface string, deny []string) error {
	if err := ipt.ClearChain("filter", egressChain); err != nil {
		return fmt.Errorf("prepare chain %s: %w", egressChain, err)
	}
	for _, c := range deny {
		if err := ipt.Append("filter", egressChain, "-d", c, "-j", "DROP"); err != nil {
			return fmt.Errorf("deny %s: %w", c, err)
		}
	}
	jump := []string{"-o", extIface, "-j", egressChain}
	if ok, err := ipt.Exists("filter", "FORWARD", jump...); err != nil {
		return fmt.Errorf("check the %s jump: %w", egressChain, err)
	} else if !ok {
		if err := ipt.Insert("filter", "FORWARD", 1, jump...); err != nil {
			return fmt.Errorf("jump to %s: %w", egressChain, err)
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
