//go:build linux

package vpn

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"

	"github.com/cybericebox/laboratory/internal/podinput"
)

type IPTablesManager struct {
	ipt     *iptables.IPTables
	wgIface string
	// ipt6 is the IPv6 table of the pod, nil when the pod has no ip6tables.
	ipt6 *iptables.IPTables
	// uplink and wgPort are what the guard of the WireGuard port was installed for ("" = not installed).
	uplink string
	wgPort int
	// input is the INPUT policy of the pod once installed (nil = not installed).
	input *podinput.Policy
}

const accessChain = "CYBERICEBOX_VPN_ACCESS"

func NewIPTablesManager(wgIface string) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	m := &IPTablesManager{ipt: ipt, wgIface: wgIface}
	if ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err == nil {
		m.ipt6 = ipt6
	}
	return m, nil
}

// GuardWireGuardPort makes the WireGuard port of the pod reachable only through the uplink interface, where the wg-demux of the
// proxy delivers the clients' packets: a packet to that port from any other interface, the lab interfaces among them, is dropped
// (see wgguard.go). It must run before the WireGuard interface is created, so that the port is never open to the lab side. The
// result says whether IPv6 was guarded too (false: the pod has no ip6tables; the guard is then IPv4 only and the caller logs it).
func (m *IPTablesManager) GuardWireGuardPort(uplink string, port int) (ipv6 bool, err error) {
	if err := installWGGuard(m.ipt, uplink, port); err != nil {
		return false, err
	}
	m.uplink, m.wgPort = uplink, port
	if m.ipt6 != nil {
		if err := installWGGuard(m.ipt6, uplink, port); err != nil {
			return false, err
		}
		ipv6 = true
	}
	return ipv6, nil
}

// inputTable is what the INPUT setup uses of go-iptables (a stand-in in the tests).
type inputTable interface{ podinput.Table }

// ProtectInput installs the INPUT policy of the pod (see internal/podinput): nothing on the lab side can talk to the pod itself,
// the WireGuard side reaches only the status page on probeAddr:probePort, the uplink is open. It must run before any interface
// of the pod comes up. The result says whether IPv6 is filtered (true) or switched off (false). Every failing step is an error and
// the pod must not start.
func (m *IPTablesManager) ProtectInput(uplink, probeAddr string, probePort int) (ipv6Filtered bool, err error) {
	p := podinput.Policy{Uplink: uplink, Probe: &podinput.Probe{Iface: m.wgIface, Addr: probeAddr, Port: probePort}}
	var v6 inputTable
	if m.ipt6 != nil {
		v6 = m.ipt6
	}
	ipv6Filtered, err = protectInput(m.ipt, v6, p, podinput.IPv6Off, func() error { return podinput.NoConntrackHelpers(podinput.ConntrackHelpers) })
	if err != nil {
		return false, err
	}
	m.input = &p
	if !ipv6Filtered {
		m.ipt6 = nil // IPv6 is off: nothing of it is filtered or guarded
	}
	return ipv6Filtered, nil
}

func protectInput(v4, v6 inputTable, p podinput.Policy, ensureOff, helpersOff func() error) (bool, error) {
	// Nothing is forwarded until the FORWARD rules are in place: the policy is DROP from the first moment.
	if err := v4.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return false, fmt.Errorf("set FORWARD DROP: %w", err)
	}
	if err := helpersOff(); err != nil {
		return false, err
	}
	if err := podinput.Install(v4, p, false); err != nil {
		return false, err
	}
	var t6 podinput.Table
	if v6 != nil {
		t6 = v6
	}
	return podinput.ProtectIPv6(t6, p, ensureOff)
}

// AllowDHCP opens DHCP on one lab interface (the pod runs a DHCP server there); DenyDHCP closes it.
func (m *IPTablesManager) AllowDHCP(iface string) error {
	if m.input == nil {
		return fmt.Errorf("the INPUT policy is not installed")
	}
	return podinput.AllowDHCP(m.ipt, iface)
}

func (m *IPTablesManager) DenyDHCP(iface string) { podinput.DenyDHCP(m.ipt, iface) }

// AllowPing lets the devices of a lab ping the pod's own address on that lab's interface (and no other address of the pod);
// DenyPing takes it away with the interface.
func (m *IPTablesManager) AllowPing(iface, addr string) error {
	if m.input == nil {
		return fmt.Errorf("the INPUT policy is not installed")
	}
	return podinput.AllowPing(m.ipt, iface, addr)
}

func (m *IPTablesManager) DenyPing(iface, addr string) { podinput.DenyPing(m.ipt, iface, addr) }

// forwardRules are the base rules of FORWARD, in the order they stand. The pod is a transparent gateway between the lab and the
// participants, with one exception: participants never reach each other through it. That drop comes first, so no conntrack state
// can let a packet from the tunnel back into the tunnel.
func (m *IPTablesManager) forwardRules() [][]string {
	return [][]string{
		{"-i", m.wgIface, "-o", m.wgIface, "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "INVALID", "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-i", m.wgIface, "-o", "lab+", "-j", accessChain},
		// Every lab behind the pod may reach every participant, from any source address: the pod is one group's, the labs of other
		// groups are not connected to it, and a lab may route its own subnets. Replies come back by conntrack.
		{"-i", "lab+", "-o", m.wgIface, "-j", "ACCEPT"},
	}
}

// SetupForwardPolicy sets the FORWARD policy to DROP, rebuilds the base rules (a restart in a surviving network namespace never
// finds an older order of them) and installs the default-deny access chain for participant-to-lab traffic. Called once on pod
// start before the reconcile loop begins; the lab-to-participant accepts of AllowLabToClients are added by the reconciler.
func (m *IPTablesManager) SetupForwardPolicy() error {
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP: %w", err)
	}
	// The FORWARD jump below references the access chain, and iptables rejects
	// even checking a rule whose target chain does not exist yet: create it
	// first (ClearChain creates it empty, i.e. deny-only).
	if err := m.ipt.ClearChain("filter", accessChain); err != nil {
		return fmt.Errorf("create access chain: %w", err)
	}
	// The policy is DROP, so the empty chain is closed while the rules are put back in order.
	if err := m.ipt.ClearChain("filter", "FORWARD"); err != nil {
		return fmt.Errorf("flush FORWARD: %w", err)
	}
	for _, r := range m.forwardRules() {
		if err := m.ipt.Append("filter", "FORWARD", r...); err != nil {
			return fmt.Errorf("iptables FORWARD %v: %w", r, err)
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
		target := "DROP"
		if rule.Action == AccessAllow {
			target = "ACCEPT"
		}
		if err := m.ipt.AppendUnique("filter", accessChain,
			"-s", rule.SourceCIDR,
			"-d", rule.DestinationCIDR,
			"-m", "comment", "--comment", accessRuleComment(rule),
			"-j", target,
		); err != nil {
			return fmt.Errorf("apply %s %s to %s: %w", rule.Action, rule.SourceCIDR, rule.DestinationCIDR, err)
		}
	}
	return nil
}

const accessRuleCommentPrefix = "cice:"

func accessRuleComment(rule AccessRule) string {
	return accessRuleCommentPrefix + rule.Identifier()
}

var accessCounterPattern = regexp.MustCompile(`(?:^|\s)-c\s+(\d+)\s+(\d+)(?:\s|$)`)

// AccessCounters reads cumulative packet/byte counters from the dedicated
// chain and associates them with stable relation IDs.
func (m *IPTablesManager) AccessCounters() (map[string]TrafficCounter, error) {
	lines, err := m.ipt.ListWithCounters("filter", accessChain)
	if err != nil {
		return nil, fmt.Errorf("list access counters: %w", err)
	}
	counters := make(map[string]TrafficCounter)
	for _, line := range lines {
		commentIndex := strings.Index(line, accessRuleCommentPrefix)
		if commentIndex < 0 {
			continue
		}
		id := line[commentIndex+len(accessRuleCommentPrefix):]
		id = strings.Trim(id, "\" '")
		if fieldEnd := strings.IndexAny(id, " \t"); fieldEnd >= 0 {
			id = id[:fieldEnd]
		}
		match := accessCounterPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		packets, packetErr := strconv.ParseInt(match[1], 10, 64)
		bytes, bytesErr := strconv.ParseInt(match[2], 10, 64)
		if packetErr != nil || bytesErr != nil {
			continue
		}
		counters[id] = TrafficCounter{Packets: packets, Bytes: bytes}
	}
	return counters, nil
}

func (m *IPTablesManager) Cleanup() {
	_ = m.ipt.ClearChain("filter", "FORWARD")
	_ = m.ipt.ClearAndDeleteChain("filter", accessChain)
	if m.uplink != "" {
		removeWGGuard(m.ipt, m.uplink, m.wgPort)
		if m.ipt6 != nil {
			removeWGGuard(m.ipt6, m.uplink, m.wgPort)
		}
	}
	if m.input != nil {
		podinput.Remove(m.ipt)
		if m.ipt6 != nil {
			podinput.Remove(m.ipt6)
		}
	}
}
