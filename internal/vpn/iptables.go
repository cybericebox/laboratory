//go:build linux

package vpn

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"

	"github.com/cybericebox/laboratory/internal/podinput"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
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
	input         *podinput.Policy
	commands      RuleCommand
	forwardMu     sync.Mutex
	forwardPlan   ForwardPlan
	forwardReady  bool
	bindings      map[string]ForwardRule
	BeforeRetire  func(flowacct.CounterSnapshot) error
	AfterRetire   func([]string)
	retirePending bool
	quiesced      bool
}

const accessChain = "CYBERICEBOX_VPN_ACCESS"

func NewIPTablesManager(wgIface string) (*IPTablesManager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("iptables.New: %w", err)
	}
	m := &IPTablesManager{ipt: ipt, wgIface: wgIface, commands: nativeRuleCommand{}}
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
		{"-i", m.wgIface, "-o", "lab+", "-j", accessChain},
		{"-i", "lab+", "-o", m.wgIface, "-j", accessChain},
		// The access gate precedes this for every VPN/lab packet, including
		// established replies. Unmatched access traffic is dropped by the gate.
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}
}

// SetupForwardPolicy closes the access gate and rebuilds the private VPN
// FORWARD chain in one commit. An empty access chain must never fall through
// to established forwarding during a surviving-network-namespace restart.
func (m *IPTablesManager) SetupForwardPolicy() error {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return err
	}
	var body strings.Builder
	body.WriteString("*filter\n:" + accessChain + " - [0:0]\n-F " + accessChain + "\n-A " + accessChain + " -j DROP\n-F FORWARD\n")
	for _, rule := range m.forwardRules() {
		fmt.Fprintf(&body, "-A FORWARD %s\n", strings.Join(rule, " "))
	}
	body.WriteString("COMMIT\n")
	if err := m.commands.Restore(context.Background(), []byte(body.String())); err != nil {
		return err
	}
	m.forwardReady = false
	return nil
}

// ReplaceAccessRules replaces the allowed symmetric bindings in one gate
// commit; detached retired counters are persisted before a cleanup commit.
func (m *IPTablesManager) ReplaceAccessRules(rules []AccessRule) error {
	plan := ForwardPlan{Decisions: rules}
	for _, r := range rules {
		if r.Action == AccessAllow {
			f, err := forwardBinding(r)
			if err != nil {
				return err
			}
			plan.Allows = append(plan.Allows, f)
		}
	}
	_, err := m.ApplyForwardPlan(context.Background(), plan)
	return err
}

const accessRuleCommentPrefix = "cice:"

func accessRuleComment(rule AccessRule) string {
	return accessRuleCommentPrefix + rule.Identifier()
}

var accessCounterPattern = regexp.MustCompile(`(?:^|\s)-c\s+(\d+)\s+(\d+)(?:\s|$)`)

// AccessCounters reads cumulative packet/byte counters from the dedicated
// chain and associates them with stable relation IDs.
func (m *IPTablesManager) AccessCounters() (map[string]TrafficCounter, error) {
	m.forwardMu.Lock()
	ready := m.forwardReady
	m.forwardMu.Unlock()
	if ready {
		snapshot, err := m.ReadPairCounters(context.Background())
		if err != nil {
			return nil, err
		}
		result := map[string]TrafficCounter{}
		for _, row := range snapshot.Rows {
			id := AccessRule{ClientName: row.Subject, LabName: row.Lab, Action: AccessAllow}.Identifier()
			result[id] = TrafficCounter{Packets: counterInt64(row.PacketsOut), Bytes: counterInt64(row.BytesOut)}
		}
		return result, nil
	}
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
		if fieldEnd := strings.IndexAny(id, " \t"); fieldEnd >= 0 {
			id = id[:fieldEnd]
		}
		// iptables -S quotes comments; trim after isolating the token so
		// the closing quote before the next argument cannot become part of the ID.
		id = strings.Trim(id, "\" '")
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
	m.forwardMu.Lock()
	if m.commands != nil {
		if saved, err := m.commands.Save(context.Background()); err == nil {
			if parsed, err := parseKernelCounters(saved, m.bindings); err == nil {
				for id := range parsed.epochs {
					for _, d := range []string{"F", "R"} {
						_ = m.ipt.ClearAndDeleteChain("filter", relationChain(id, d))
					}
				}
			}
		}
	}
	m.forwardReady = false
	m.bindings = nil
	m.forwardMu.Unlock()
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

func counterInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
