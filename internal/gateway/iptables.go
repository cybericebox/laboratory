package gateway

import (
	"fmt"

	"github.com/coreos/go-iptables/iptables"

	"github.com/cybericebox/laboratory/internal/egress"
	"github.com/cybericebox/laboratory/internal/podinput"
)

// netfilter is the part of go-iptables the gateway uses (a fake stands in for it in the tests).
type netfilter interface {
	ChangePolicy(table, chain, target string) error
	AppendUnique(table, chain string, rulespec ...string) error
	Append(table, chain string, rulespec ...string) error
	Insert(table, chain string, pos int, rulespec ...string) error
	Delete(table, chain string, rulespec ...string) error
	Exists(table, chain string, rulespec ...string) (bool, error)
	ClearChain(table, chain string) error
	ClearAndDeleteChain(table, chain string) error
	ChainExists(table, chain string) (bool, error)
}

type IPTablesManager struct {
	ipt      netfilter
	extIface string
	// IPv6Filtered is true when the egress filter was installed for IPv6 as well; false means the pod has no ip6tables
	// and forwards no IPv6 (the caller logs it).
	IPv6Filtered bool
	newIPv6      func() (netfilter, error)
	// disableIPv6 switches the IPv6 stack of the pod off (a stand-in in the tests, which must not touch the host).
	disableIPv6 func() error
	// ip6 is the IPv6 table of the pod once SetupFilter found one.
	ip6 netfilter
	// inputInstalled is true once the INPUT policy stands.
	inputInstalled bool
}

func NewIPTablesManager(extIface string) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	return &IPTablesManager{ipt: ipt, extIface: extIface, disableIPv6: podinput.IPv6Off, newIPv6: func() (netfilter, error) {
		return iptables.NewWithProtocol(iptables.ProtocolIPv6)
	}}, nil
}

const (
	// labIfaces matches every lab interface of the gateway pod (lab<N>).
	labIfaces = "lab+"
	// sourceChain holds one rule per lab that drops what a lab interface sends with a source outside the lab's own subnet.
	sourceChain = "LABSRC"
)

// egressChains are the two names the egress filter alternates between, so a rebuild never empties the chain in use.
var egressChains = [2]string{"LABEGRESS", "LABEGRESS2"}

// SetupFilter installs the whole forwarding filter, in the order that is never open: the policy is DROP, the egress filter
// and the anti-spoof chain come first in FORWARD, and only then the rules that accept lab traffic towards the outside. It
// is safe to run again in a pod whose network namespace outlives the process (a container restart): the old filter keeps
// working until the new one is in place, and the rule of earlier versions that accepted everything leaving is removed.
func (m *IPTablesManager) SetupFilter() error {
	// The pod answers nothing on the lab side: the INPUT policy is first, before any lab interface is configured.
	if err := m.protectInput(); err != nil {
		return err
	}
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP: %w", err)
	}
	if err := setupEgressChain(m.ipt, m.extIface, egress.DenyV4); err != nil {
		return err
	}
	if err := m.setupSourceChain(); err != nil {
		return err
	}
	if m.ip6 != nil { // the FORWARD policy of IPv6 is DROP since protectInput
		if err := setupEgressChain(m.ip6, m.extIface, egress.DenyV6); err != nil {
			return err
		}
		m.IPv6Filtered = true
	}
	return m.setupForwardRules()
}

// protectInput installs the INPUT policy (see internal/podinput) for IPv4 and for IPv6 (filtered when the pod has a working
// ip6tables, switched off otherwise). The gateway has no listener of its own; its DHCP servers are opened per lab interface
// (AllowDHCP). Any failing step is an error and the pod must not start.
func (m *IPTablesManager) protectInput() error {
	p := podinput.Policy{Uplink: m.extIface}
	if err := podinput.Install(m.ipt, p, false); err != nil {
		return err
	}
	m.inputInstalled = true
	if m.newIPv6 != nil && m.ip6 == nil {
		if ip6, err := m.newIPv6(); err == nil {
			m.ip6 = ip6
		}
	}
	var t6 podinput.Table
	if m.ip6 != nil {
		t6 = m.ip6
	}
	filtered, err := podinput.ProtectIPv6(t6, p, m.disableIPv6)
	if err != nil {
		return err
	}
	if !filtered {
		m.ip6 = nil
	}
	return nil
}

// AllowDHCP opens DHCP on one lab interface (the pod runs a DHCP server there); DenyDHCP closes it.
func (m *IPTablesManager) AllowDHCP(iface string) error { return podinput.AllowDHCP(m.ipt, iface) }

func (m *IPTablesManager) DenyDHCP(iface string) { podinput.DenyDHCP(m.ipt, iface) }

// AllowPing lets the devices of a lab ping the pod's own address on that lab's interface (and no other address of the pod);
// DenyPing takes it away with the interface.
func (m *IPTablesManager) AllowPing(iface, addr string) error {
	return podinput.AllowPing(m.ipt, iface, addr)
}

func (m *IPTablesManager) DenyPing(iface, addr string) { podinput.DenyPing(m.ipt, iface, addr) }

// RemoveInput takes the INPUT policy off (the pod is stopping).
func (m *IPTablesManager) RemoveInput() {
	if !m.inputInstalled {
		return
	}
	podinput.Remove(m.ipt)
	if m.ip6 != nil {
		podinput.Remove(m.ip6)
	}
}

// setupSourceChain makes FORWARD send every lab interface through LABSRC first.
func (m *IPTablesManager) setupSourceChain() error {
	if err := m.ensureChain(sourceChain); err != nil {
		return err
	}
	return ensureFirst(m.ipt, "-i", labIfaces, "-j", sourceChain)
}

// ensureChain creates the chain when it does not exist and leaves its rules when it does (ClearChain would empty it).
func (m *IPTablesManager) ensureChain(chain string) error {
	ok, err := m.ipt.ChainExists("filter", chain)
	if err != nil {
		return fmt.Errorf("check chain %s: %w", chain, err)
	}
	if ok {
		return nil
	}
	if err := m.ipt.ClearChain("filter", chain); err != nil { // creates a missing chain
		return fmt.Errorf("create chain %s: %w", chain, err)
	}
	return nil
}

// ensureFirst puts a FORWARD rule at the top, once. The jumps are inserted in the order the filter needs them: the
// source check is installed after the egress jump, so it ends up in front of it.
func ensureFirst(ipt netfilter, rule ...string) error {
	if ok, err := ipt.Exists("filter", "FORWARD", rule...); err != nil {
		return fmt.Errorf("check the FORWARD rule %v: %w", rule, err)
	} else if ok {
		return nil
	}
	if err := ipt.Insert("filter", "FORWARD", 1, rule...); err != nil {
		return fmt.Errorf("insert the FORWARD rule %v: %w", rule, err)
	}
	return nil
}

// setupForwardRules installs the rules that let lab traffic leave and its replies return. They come after the filters, and
// only lab interfaces may use them.
func (m *IPTablesManager) setupForwardRules() error {
	// The rule of earlier versions: it accepted whatever left through the external interface, from any interface.
	for {
		ok, err := m.ipt.Exists("filter", "FORWARD", "-o", m.extIface, "-j", "ACCEPT")
		if err != nil || !ok {
			break
		}
		if err := m.ipt.Delete("filter", "FORWARD", "-o", m.extIface, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("remove the open FORWARD rule: %w", err)
		}
	}
	rules := [][]string{
		// lab{N} -> internet
		{"-i", labIfaces, "-o", m.extIface, "-j", "ACCEPT"},
		// return traffic from internet -> lab{N}
		{"-i", m.extIface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}
	for _, r := range rules {
		if err := m.ipt.AppendUnique("filter", "FORWARD", r...); err != nil {
			return fmt.Errorf("iptables FORWARD %v: %w", r, err)
		}
	}
	return nil
}

// setupEgressChain builds the destination filter of the traffic the gateway forwards to the outside: every range of deny is
// dropped in a chain that FORWARD jumps to first for everything leaving through the external interface. The new chain is
// filled while the old one still runs, the jump is switched, and the old one is emptied after: there is no moment without a
// filter. The gateway's own traffic (its API client) is OUTPUT, not FORWARD, and is not touched.
func setupEgressChain(ipt netfilter, extIface string, deny []string) error {
	// Both chains must exist before the jump checks: iptables-nft fails a rule check that names a missing chain (exit 2)
	// instead of answering "no". A missing chain is created empty, which is safe while nothing jumps to it.
	for _, c := range egressChains {
		ok, err := ipt.ChainExists("filter", c)
		if err != nil {
			return fmt.Errorf("check chain %s: %w", c, err)
		}
		if !ok {
			if err := ipt.ClearChain("filter", c); err != nil { // creates a missing chain
				return fmt.Errorf("create chain %s: %w", c, err)
			}
		}
	}
	active := ""
	for _, c := range egressChains {
		if ok, err := ipt.Exists("filter", "FORWARD", "-o", extIface, "-j", c); err != nil {
			return fmt.Errorf("check the %s jump: %w", c, err)
		} else if ok {
			active = c
		}
	}
	next := egressChains[0]
	if active == next {
		next = egressChains[1]
	}
	if err := ipt.ClearChain("filter", next); err != nil {
		return fmt.Errorf("prepare chain %s: %w", next, err)
	}
	for _, c := range deny {
		if err := ipt.Append("filter", next, "-d", c, "-j", "DROP"); err != nil {
			return fmt.Errorf("deny %s: %w", c, err)
		}
	}
	if err := ipt.Insert("filter", "FORWARD", 1, "-o", extIface, "-j", next); err != nil {
		return fmt.Errorf("jump to %s: %w", next, err)
	}
	for _, c := range egressChains {
		if c == next {
			continue
		}
		for {
			ok, err := ipt.Exists("filter", "FORWARD", "-o", extIface, "-j", c)
			if err != nil || !ok {
				break
			}
			if err := ipt.Delete("filter", "FORWARD", "-o", extIface, "-j", c); err != nil {
				return fmt.Errorf("remove the old %s jump: %w", c, err)
			}
		}
		if active == c {
			_ = ipt.ClearChain("filter", c)
		}
	}
	return nil
}

// AddAntiSpoof makes the gateway drop what a lab sends with a source outside the lab's own subnet, so lab A cannot send a
// packet that looks like lab B's and have the reply sent back into lab B. The rule sits in LABSRC, which FORWARD consults
// before it accepts anything.
func (m *IPTablesManager) AddAntiSpoof(iface, labCIDR string) error {
	return m.ipt.AppendUnique("filter", sourceChain, "-i", iface, "!", "-s", labCIDR, "-j", "DROP")
}

func (m *IPTablesManager) DelAntiSpoof(iface, labCIDR string) {
	_ = m.ipt.Delete("filter", sourceChain, "-i", iface, "!", "-s", labCIDR, "-j", "DROP")
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

// BlockLab closes the physical leg before removing any old source guard. It
// stays closed through configuration failures; UnblockLab follows secured NAT.
func (m *IPTablesManager) BlockLab(iface string) error {
	spec := []string{"-i", iface, "-j", "DROP"}
	exists, err := m.ipt.Exists("filter", sourceChain, spec...)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return m.ipt.Insert("filter", sourceChain, 1, spec...)
}
func (m *IPTablesManager) UnblockLab(iface string) error {
	spec := []string{"-i", iface, "-j", "DROP"}
	exists, err := m.ipt.Exists("filter", sourceChain, spec...)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return m.ipt.Delete("filter", sourceChain, spec...)
}
