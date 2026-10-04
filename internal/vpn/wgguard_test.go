package vpn

import (
	"strconv"
	"strings"
	"testing"
)

// fakeInput keeps the INPUT chain as rule specs in order and evaluates a packet against it like netfilter does: the first
// rule that matches decides (the policy is ACCEPT, as in a pod). After every change it checks that the guard is never open: a
// packet to the WireGuard port from a lab interface is not accepted at any moment.
type fakeInput struct {
	t     *testing.T
	rules [][]string
	port  int
}

func (f *fakeInput) Exists(_, chain string, rule ...string) (bool, error) {
	for _, r := range f.rules {
		if strings.Join(r, " ") == strings.Join(rule, " ") {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeInput) Insert(_, chain string, pos int, rule ...string) error {
	if chain != "INPUT" {
		f.t.Fatalf("rule inserted into %s, want INPUT", chain)
	}
	rs := append([][]string{}, f.rules[:pos-1]...)
	rs = append(rs, append([]string{}, rule...))
	f.rules = append(rs, f.rules[pos-1:]...)
	f.check()
	return nil
}

func (f *fakeInput) Delete(_, _ string, rule ...string) error {
	for i, r := range f.rules {
		if strings.Join(r, " ") == strings.Join(rule, " ") {
			f.rules = append(append([][]string{}, f.rules[:i]...), f.rules[i+1:]...)
			f.check()
			return nil
		}
	}
	return nil
}

// verdict is what INPUT does with a UDP packet to dport that arrives on iface.
func (f *fakeInput) verdict(iface string, dport int) string {
	for _, r := range f.rules {
		match, target := true, ""
		for i := 0; i < len(r); i++ {
			switch r[i] {
			case "-i":
				match = match && r[i+1] == iface
				i++
			case "-p":
				match = match && r[i+1] == "udp"
				i++
			case "--dport":
				match = match && r[i+1] == strconv.Itoa(dport)
				i++
			case "-j":
				target = r[i+1]
				i++
			}
		}
		if match {
			return target
		}
	}
	return "ACCEPT"
}

// check: with the guard partly built or torn down, a lab interface never gets through to the WireGuard port. (Before the first
// rule exists the port is open to everyone, which is why InitServer installs the guard before the interface is created.)
func (f *fakeInput) check() {
	f.t.Helper()
	if len(f.rules) == 0 {
		return
	}
	for _, iface := range []string{"lab1", "lab42", "wg0", "lo"} {
		if v := f.verdict(iface, f.port); v == "ACCEPT" {
			f.t.Fatalf("rules %v accept a packet to the WireGuard port from %s", f.rules, iface)
		}
	}
}

func TestWGGuardOnlyTheUplinkReachesThePort(t *testing.T) {
	f := &fakeInput{t: t, port: 51820}
	if err := installWGGuard(f, "eth0", 51820); err != nil {
		t.Fatal(err)
	}
	if got := f.verdict("eth0", 51820); got != "ACCEPT" {
		t.Errorf("the uplink: %s, want ACCEPT (the demux path must work)", got)
	}
	for _, iface := range []string{"lab1", "lab2", "lab99", "wg0", "lo", "eth1"} {
		if got := f.verdict(iface, 51820); got != "DROP" {
			t.Errorf("%s: %s, want DROP", iface, got)
		}
	}
	// Other traffic is not the guard's business: another port or the same port on another protocol is left alone.
	if got := f.verdict("lab1", 8080); got != "ACCEPT" {
		t.Errorf("another port from a lab interface: %s, want ACCEPT (untouched)", got)
	}
}

func TestWGGuardRulesAreTheExpectedSpecs(t *testing.T) {
	accept, drop := wgGuardRules("eth0", 51820)
	if got, want := strings.Join(accept, " "), "-i eth0 -p udp --dport 51820 -j ACCEPT"; got != want {
		t.Errorf("accept rule %q, want %q", got, want)
	}
	if got, want := strings.Join(drop, " "), "-p udp --dport 51820 -j DROP"; got != want {
		t.Errorf("drop rule %q, want %q", got, want)
	}
}

func TestWGGuardOrderAndIdempotence(t *testing.T) {
	f := &fakeInput{t: t, port: 51820}
	for i := 0; i < 3; i++ {
		if err := installWGGuard(f, "eth0", 51820); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.rules) != 2 {
		t.Fatalf("rules after three installs: %v, want 2", f.rules)
	}
	if !strings.Contains(strings.Join(f.rules[0], " "), "-j ACCEPT") || !strings.Contains(strings.Join(f.rules[1], " "), "-j DROP") {
		t.Errorf("order %v: the accept on the uplink must come before the drop", f.rules)
	}
}

func TestWGGuardFollowsThePort(t *testing.T) {
	f := &fakeInput{t: t, port: 4000}
	if err := installWGGuard(f, "net1", 4000); err != nil {
		t.Fatal(err)
	}
	if f.verdict("net1", 4000) != "ACCEPT" || f.verdict("lab1", 4000) != "DROP" {
		t.Errorf("a custom uplink and port are not honoured: %v", f.rules)
	}
	if f.verdict("lab1", 51820) != "ACCEPT" {
		t.Errorf("the default port must be left alone when another port is configured")
	}
}

func TestWGGuardRemoval(t *testing.T) {
	f := &fakeInput{t: t, port: 51820}
	if err := installWGGuard(f, "eth0", 51820); err != nil {
		t.Fatal(err)
	}
	removeWGGuard(f, "eth0", 51820)
	if len(f.rules) != 0 {
		t.Errorf("rules left after removal: %v", f.rules)
	}
}
