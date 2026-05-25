package main

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"
)

type IPTablesManager struct {
	ipt          *iptables.IPTables
	clientSubnet string
}

func newIPTablesManager(clientSubnet *net.IPNet) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	return &IPTablesManager{ipt: ipt, clientSubnet: clientSubnet.String()}, nil
}

// SetupForwardPolicy installs invariant FORWARD rules:
//
//	ESTABLISHED,RELATED → ACCEPT
//	src ∈ clientSubnet  → ACCEPT (user→lab)
//	dst ∈ clientSubnet  → ACCEPT (lab→user)
//	DROP
func (m *IPTablesManager) SetupForwardPolicy() error {
	rules := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-s", m.clientSubnet, "-j", "ACCEPT"},
		{"-d", m.clientSubnet, "-j", "ACCEPT"},
	}
	for _, rule := range rules {
		if err := m.ipt.AppendUnique("filter", "FORWARD", rule...); err != nil {
			return fmt.Errorf("append FORWARD rule %v: %w", rule, err)
		}
	}
	exists, _ := m.ipt.Exists("filter", "FORWARD", "-j", "DROP")
	if !exists {
		return m.ipt.Append("filter", "FORWARD", "-j", "DROP")
	}
	return nil
}

func (m *IPTablesManager) Cleanup() {
	rules := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-s", m.clientSubnet, "-j", "ACCEPT"},
		{"-d", m.clientSubnet, "-j", "ACCEPT"},
		{"-j", "DROP"},
	}
	for _, rule := range rules {
		_ = m.ipt.Delete("filter", "FORWARD", rule...)
	}
}
