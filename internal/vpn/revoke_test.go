package vpn

import (
	"net/netip"
	"testing"
)

func flow(id uint32, src, dst string) ConnFlow {
	return ConnFlow{ID: id, Key: int(id), Src: netip.MustParseAddr(src), Dst: netip.MustParseAddr(dst)}
}

func TestRevokedFlows(t *testing.T) {
	labs := []string{"10.8.1.0/24", "10.8.2.0/24"}
	rules := []AccessRule{
		{ClientName: "alice", LabName: "l1", SourceCIDR: "10.7.0.2/32", DestinationCIDR: "10.8.1.0/24", Action: AccessAllow},
		{ClientName: "alice", LabName: "l2", SourceCIDR: "10.7.0.2/32", DestinationCIDR: "10.8.2.0/24", Action: AccessDeny},
		{ClientName: "bob", LabName: "l1", SourceCIDR: "10.7.0.3", DestinationCIDR: "10.8.1.0/24", Action: AccessDeny},
	}
	flows := []ConnFlow{
		flow(1, "10.7.0.2", "10.8.1.5"),   // alice to l1: allowed
		flow(2, "10.7.0.2", "10.8.2.5"),   // alice to l2: denied now
		flow(3, "10.7.0.3", "10.8.1.9"),   // bob to l1: denied now
		flow(4, "10.7.0.9", "10.8.1.9"),   // a client that no rule names (deleted): cut
		flow(5, "10.7.0.2", "10.7.0.1"),   // to the VPN pod itself, not a lab: untouched
		flow(6, "10.8.1.5", "10.8.2.5"),   // between lab networks: not a client flow
		flow(7, "10.7.0.2", "10.8.1.200"), // alice to l1, another host: allowed
	}
	var got []uint32
	for _, f := range RevokedFlows(flows, labs, rules) {
		got = append(got, f.ID)
	}
	want := []uint32{2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestRevokedFlowsWithNoLabsOrNoRules(t *testing.T) {
	flows := []ConnFlow{flow(1, "10.7.0.2", "10.8.1.5")}
	if RevokedFlows(flows, nil, nil) != nil {
		t.Fatal("without lab networks nothing is governed")
	}
	// No allow rule at all: every client-to-lab connection is revoked (a group with its access removed).
	if got := RevokedFlows(flows, []string{"10.8.1.0/24"}, nil); len(got) != 1 {
		t.Fatalf("default deny: %v", got)
	}
}
