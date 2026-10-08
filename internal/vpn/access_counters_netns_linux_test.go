//go:build linux

package vpn

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
)

func TestNetnsAccessCountersRoundTripQuotedComment(t *testing.T) {
	nstest.Require(t)
	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	rule := AccessRule{ClientName: "p1", LabName: "l1", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.8.1.0/24", Action: AccessAllow}
	nstest.Run(t, "", "iptables", "-t", "filter", "-I", accessChain, "1",
		"-s", rule.SourceCIDR, "-d", rule.DestinationCIDR,
		"-m", "comment", "--comment", accessRuleComment(rule), "-j", "ACCEPT", "-c", "7", "700")
	counters, err := m.AccessCounters()
	if err != nil {
		t.Fatal(err)
	}
	if got := counters[rule.Identifier()]; got.Packets != 7 || got.Bytes != 700 {
		t.Fatalf("real iptables counter did not match its rule identity: %v, want key %q with 7/700", counters, rule.Identifier())
	}
}
