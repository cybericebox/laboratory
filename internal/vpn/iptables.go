//go:build linux

package vpn

import (
	"fmt"
	"regexp"
	"strconv"
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
