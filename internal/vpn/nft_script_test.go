package vpn

import (
	"strings"
	"testing"
)

func scriptFor(t *testing.T, rules ...ForwardRule) string {
	t.Helper()
	s, err := nftPlanScript(ForwardPlan{Allows: rules}, func(string) bool { return true }, func() (string, error) { return "0011223344556677", nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func nftRule(t *testing.T, client, lab, iface string, open bool) ForwardRule {
	t.Helper()
	r, err := forwardBinding(AccessRule{ClientName: client, LabName: lab, SourceCIDR: "10.8.0." + client + "/32", DestinationCIDR: "10.9." + iface[3:] + ".0/24", LabInterface: iface, Action: AccessAllow, LabInitiated: open})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A lab that may not start connections is forwarded only as replies and is tracked; the stateless sets do not mention it.
func TestNFTBlockedLabIsTrackedAndOnlyRepliesPass(t *testing.T) {
	s := scriptFor(t, nftRule(t, "5", "a", "lab1", false))
	for _, want := range []string{
		`add element ip cicevpn allow_fwd { "lab1" . 10.8.0.5 . 10.9.1.0/24 }`,
		`add element ip cicevpn reply_rev { "lab1" . 10.9.1.0/24 . 10.8.0.5 }`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"allow_rev {", "labnets {", "open_clients {"} {
		if strings.Contains(s, bad) {
			t.Errorf("a blocked lab is in %q:\n%s", bad, s)
		}
	}
}

// A lab that may start connections keeps the stateless, untracked path of before.
func TestNFTOpenLabIsUntrackedAndStateless(t *testing.T) {
	s := scriptFor(t, nftRule(t, "5", "a", "lab1", true))
	for _, want := range []string{
		`add element ip cicevpn allow_rev { "lab1" . 10.9.1.0/24 . 10.8.0.5 }`,
		`add element ip cicevpn labnets { 10.9.1.0/24 }`,
		`add element ip cicevpn open_clients { "lab1" . 10.8.0.5 }`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "reply_rev {") {
		t.Errorf("an open lab must not be restricted to replies:\n%s", s)
	}
}

// One client, two labs, one open and one blocked: the notrack key includes the lab interface, so the blocked lab's replies are tracked.
func TestNFTSwitchIsPerLabForTheSameClient(t *testing.T) {
	s := scriptFor(t, nftRule(t, "5", "a", "lab1", true), nftRule(t, "5", "b", "lab2", false))
	if !strings.Contains(s, `open_clients { "lab1" . 10.8.0.5 }`) || strings.Contains(s, `open_clients { "lab2"`) {
		t.Errorf("the open lab only:\n%s", s)
	}
	if !strings.Contains(s, `reply_rev { "lab2" . 10.9.2.0/24 . 10.8.0.5 }`) || !strings.Contains(s, `allow_rev { "lab1" . 10.9.1.0/24 . 10.8.0.5 }`) {
		t.Errorf("per-lab sets:\n%s", s)
	}
	base := nftBaseScript("wg0")
	if !strings.Contains(base, `prerouting iifname . ip daddr @open_clients notrack`) {
		t.Errorf("the lab-to-client notrack must be keyed by lab interface and client:\n%s", base)
	}
}

// Every plan empties all the permission and notrack sets in the same transaction, so a switch change is atomic.
func TestNFTPlanReplacesAllSetsAtOnce(t *testing.T) {
	s := scriptFor(t)
	for _, set := range []string{"allow_fwd", "allow_rev", "reply_rev", "labnets", "open_clients"} {
		if !strings.Contains(s, "flush set ip cicevpn "+set+"\n") {
			t.Errorf("set %s is not flushed:\n%s", set, s)
		}
	}
}

func TestNFTBaseScriptGate(t *testing.T) {
	base := nftBaseScript("wg0")
	for _, want := range []string{
		"ct state invalid drop",
		`oifname "wg0" iifname . ip saddr . ip daddr @reply_rev ct state established,related ct direction reply jump count_rev`,
		`oifname "wg0" iifname . ip saddr . ip daddr @allow_rev jump count_rev`,
		"policy drop",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base ruleset lacks %q", want)
		}
	}
	// replies are matched by a ct rule that comes only with reply_rev: the open path has none
	if strings.Contains(strings.Split(base, "@allow_rev")[1][:40], "ct state") {
		t.Error("the open path must stay stateless")
	}
}
