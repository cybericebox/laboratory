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

// ProtectInput installs the INPUT policy of the pod (see internal/podinput): nothing on the lab side can talk to the pod itself,
// the WireGuard side reaches only the status page on probeAddr:probePort, the uplink is open. It must run before any interface
// of the pod comes up. The result says whether IPv6 was covered too (false: the pod has no ip6tables).
func (m *IPTablesManager) ProtectInput(uplink, probeAddr string, probePort int) (ipv6 bool, err error) {
	p := podinput.Policy{Uplink: uplink, Probe: &podinput.Probe{Iface: m.wgIface, Addr: probeAddr, Port: probePort}}
	if err := podinput.Install(m.ipt, p, false); err != nil {
		return false, err
	}
	m.input = &p
	if m.ipt6 != nil {
		if err := podinput.Install(m.ipt6, p, true); err != nil {
			return false, err
		}
		ipv6 = true
	}
	return ipv6, nil
}

// AllowDHCP opens DHCP on one lab interface (the pod runs a DHCP server there); DenyDHCP closes it.
func (m *IPTablesManager) AllowDHCP(iface string) error {
	if m.input == nil {
		return fmt.Errorf("the INPUT policy is not installed")
	}
	return podinput.AllowDHCP(m.ipt, iface)
}

func (m *IPTablesManager) DenyDHCP(iface string) { podinput.DenyDHCP(m.ipt, iface) }

// forwardRules are the base rules of FORWARD, in the order they stand. The pod is a transparent gateway between the lab and the
// participants, with one exception: participants never reach each other through it. That drop comes first, so no conntrack state
// can let a packet from the tunnel back into the tunnel.
func (m *IPTablesManager) forwardRules() [][]string {
	return [][]string{
		{"-i", m.wgIface, "-o", m.wgIface, "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "INVALID", "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-i", m.wgIface, "-o", "lab+", "-j", accessChain},
	}
}

// SetupForwardPolicy sets the FORWARD policy to DROP, rebuilds the base rules (a restart in a surviving network namespace never
// finds an older order of them) and installs the default-deny access chain for participant-to-lab traffic. Called once on pod
// start before the reconcile loop begins; the lab-to-participant accepts of AllowLabToClients are added by the reconciler.
func (m *IPTablesManager) SetupForwardPolicy() error {
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP: %w", err)
	}
	if m.ipt6 != nil {
		// The pod forwards no IPv6; the policy keeps it so.
		if err := m.ipt6.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
			return fmt.Errorf("set IPv6 FORWARD DROP: %w", err)
		}
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

// labToClients is the accept of what a lab sends to the participants: a new connection from a device of the lab's own subnet.
// The source check keeps a device from sending as a device of another lab and having the participant answer into that one.
func (m *IPTablesManager) labToClients(iface, labCIDR string) []string {
	return []string{"-i", iface, "-s", labCIDR, "-o", m.wgIface, "-j", "ACCEPT"}
}

// AllowLabToClients lets the devices of one lab reach every participant (the replies of the participants go back by conntrack).
func (m *IPTablesManager) AllowLabToClients(iface, labCIDR string) error {
	return m.ipt.AppendUnique("filter", "FORWARD", m.labToClients(iface, labCIDR)...)
}

func (m *IPTablesManager) RevokeLabToClients(iface, labCIDR string) {
	_ = m.ipt.Delete("filter", "FORWARD", m.labToClients(iface, labCIDR)...)
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
