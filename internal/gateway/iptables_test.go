package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/egress"
)

// fakeNetfilter keeps chains as lists of rule strings, in order, and checks after every change that the filter is never open: whenever a
// rule accepts lab traffic towards the outside, a jump to a non-empty egress chain stands before it.
type fakeNetfilter struct {
	t      *testing.T
	chains map[string][]string
	policy string
}

func newFake(t *testing.T) *fakeNetfilter {
	return &fakeNetfilter{t: t, chains: map[string][]string{"FORWARD": nil}, policy: "ACCEPT"}
}

func (f *fakeNetfilter) key(rule []string) string { return strings.Join(rule, " ") }

func (f *fakeNetfilter) ChangePolicy(_, chain, target string) error {
	if chain == "FORWARD" {
		f.policy = target
	}
	return nil
}
func (f *fakeNetfilter) ChainExists(_, chain string) (bool, error) {
	_, ok := f.chains[chain]
	return ok, nil
}
func (f *fakeNetfilter) ClearChain(_, chain string) error {
	f.chains[chain] = nil
	f.check()
	return nil
}
func (f *fakeNetfilter) ClearAndDeleteChain(_, chain string) error {
	delete(f.chains, chain)
	return nil
}
func (f *fakeNetfilter) Exists(_, chain string, rule ...string) (bool, error) {
	// Like iptables-nft: checking a rule whose -j target is a chain that does not exist fails (exit 2), it is not "false".
	for i := 0; i+1 < len(rule); i++ {
		if rule[i] == "-j" && rule[i+1] != "ACCEPT" && rule[i+1] != "DROP" && rule[i+1] != "RETURN" {
			if _, ok := f.chains[rule[i+1]]; !ok {
				return false, fmt.Errorf("exit status 2: Chain '%s' does not exist", rule[i+1])
			}
		}
	}
	for _, r := range f.chains[chain] {
		if r == f.key(rule) {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeNetfilter) Append(_, chain string, rule ...string) error {
	f.chains[chain] = append(f.chains[chain], f.key(rule))
	f.check()
	return nil
}
func (f *fakeNetfilter) AppendUnique(table, chain string, rule ...string) error {
	if ok, _ := f.Exists(table, chain, rule...); ok {
		return nil
	}
	return f.Append(table, chain, rule...)
}
func (f *fakeNetfilter) Insert(_, chain string, pos int, rule ...string) error {
	rs := f.chains[chain]
	rs = append(rs[:pos-1:pos-1], append([]string{f.key(rule)}, rs[pos-1:]...)...)
	f.chains[chain] = rs
	f.check()
	return nil
}
func (f *fakeNetfilter) Delete(_, chain string, rule ...string) error {
	rs := f.chains[chain]
	for i, r := range rs {
		if r == f.key(rule) {
			f.chains[chain] = append(rs[:i:i], rs[i+1:]...)
			f.check()
			return nil
		}
	}
	return fmt.Errorf("no such rule")
}

func (f *fakeNetfilter) check() {
	f.t.Helper()
	for i, r := range f.chains["FORWARD"] {
		if !strings.HasSuffix(r, "-j ACCEPT") || strings.Contains(r, "ESTABLISHED") {
			continue
		}
		ok := false
		for _, before := range f.chains["FORWARD"][:i] {
			for _, c := range egressChains {
				if before == "-o eth0 -j "+c && len(f.chains[c]) > 0 {
					ok = true
				}
			}
		}
		if !ok {
			f.t.Fatalf("FORWARD accepts %q with no egress filter before it: %v", r, f.chains["FORWARD"])
		}
	}
}

func manager(f *fakeNetfilter) *IPTablesManager {
	return &IPTablesManager{ipt: f, extIface: "eth0", disableIPv6: func() error { return nil }}
}

func TestSetupFilterOrderAndContent(t *testing.T) {
	f := newFake(t)
	m := manager(f)
	if err := m.SetupFilter(); err != nil {
		t.Fatal(err)
	}
	if f.policy != "DROP" {
		t.Errorf("policy = %s", f.policy)
	}
	fw := f.chains["FORWARD"]
	if fw[len(fw)-2] != "-i lab+ -o eth0 -j ACCEPT" || !strings.Contains(fw[len(fw)-1], "ESTABLISHED") {
		t.Errorf("the accepting rules come last and only for lab interfaces: %v", fw)
	}
	if got := len(f.chains["LABEGRESS"]); got != len(egress.DenyV4) {
		t.Errorf("deny rules = %d, want %d", got, len(egress.DenyV4))
	}
	has := func(r string) bool { ok, _ := f.Exists("filter", "FORWARD", strings.Fields(r)...); return ok }
	if !has("-i lab+ -j LABSRC") {
		t.Errorf("FORWARD must pass lab interfaces through the source check: %v", fw)
	}
	if has("-o eth0 -j ACCEPT") {
		t.Error("no rule may accept everything leaving, whatever the input interface")
	}
}

// A restart in a pod whose rules survive: the rule of earlier versions goes, the filter never opens, and the chains alternate.
func TestSetupFilterAgainAfterRestart(t *testing.T) {
	f := newFake(t)
	// the state an earlier version left: its egress chain, its jump and the open rule
	f.chains["LABEGRESS"] = []string{"-d 10.0.0.0/8 -j DROP"}
	f.chains["FORWARD"] = []string{"-o eth0 -j LABEGRESS", "-o eth0 -j ACCEPT", "-i eth0 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT"}
	m := manager(f)
	for i := 0; i < 3; i++ {
		if err := m.SetupFilter(); err != nil {
			t.Fatal(err)
		}
		jumps := 0
		for _, r := range f.chains["FORWARD"] {
			if strings.HasPrefix(r, "-o eth0 -j LABEGRESS") {
				jumps++
			}
			if r == "-o eth0 -j ACCEPT" {
				t.Fatal("the open rule must be gone")
			}
		}
		if jumps != 1 {
			t.Fatalf("run %d: %d egress jumps: %v", i, jumps, f.chains["FORWARD"])
		}
	}
}

func TestAntiSpoofIsPerLabAndRemovable(t *testing.T) {
	f := newFake(t)
	m := manager(f)
	if err := m.SetupFilter(); err != nil {
		t.Fatal(err)
	}
	if err := m.AddAntiSpoof("lab3", "10.8.3.0/24"); err != nil {
		t.Fatal(err)
	}
	if err := m.AddAntiSpoof("lab3", "10.8.3.0/24"); err != nil { // idempotent
		t.Fatal(err)
	}
	if got := f.chains[sourceChain]; len(got) != 1 || got[0] != "-i lab3 ! -s 10.8.3.0/24 -j DROP" {
		t.Errorf("source chain: %v", got)
	}
	m.DelAntiSpoof("lab3", "10.8.3.0/24")
	if len(f.chains[sourceChain]) != 0 {
		t.Errorf("the rule must go with the lab: %v", f.chains[sourceChain])
	}
}

// The IPv6 filter is installed when the pod has ip6tables and reported when it does not.
func TestIPv6FilterReported(t *testing.T) {
	f4, f6 := newFake(t), newFake(t)
	m := manager(f4)
	m.newIPv6 = func() (netfilter, error) { return f6, nil }
	if err := m.SetupFilter(); err != nil {
		t.Fatal(err)
	}
	if !m.IPv6Filtered || len(f6.chains["LABEGRESS"]) != len(egress.DenyV6) || f6.policy != "DROP" {
		t.Errorf("ipv6 filtered=%v rules=%d policy=%s", m.IPv6Filtered, len(f6.chains["LABEGRESS"]), f6.policy)
	}
	m2 := manager(newFake(t))
	m2.newIPv6 = func() (netfilter, error) { return nil, fmt.Errorf("no ip6tables") }
	if err := m2.SetupFilter(); err != nil || m2.IPv6Filtered {
		t.Errorf("err=%v filtered=%v", err, m2.IPv6Filtered)
	}
}

// A security setup step that fails stops the start: SetupFilter returns the error and the binary exits non-zero.
func TestSetupFilterFailsClosed(t *testing.T) {
	m := manager(newFake(t))
	m.disableIPv6 = func() error { return fmt.Errorf("IPv6 is on") }
	if err := m.SetupFilter(); err == nil {
		t.Errorf("no ip6tables and IPv6 on: SetupFilter went on")
	}
	m = manager(newFake(t))
	m.newIPv6 = func() (netfilter, error) { return &failPolicy{newFake(t)}, nil }
	m.disableIPv6 = func() error { return fmt.Errorf("IPv6 is on") }
	if err := m.SetupFilter(); err == nil {
		t.Errorf("a broken ip6tables and IPv6 on: SetupFilter went on")
	}
	m = manager(newFake(t))
	m.newIPv6 = func() (netfilter, error) { return &failPolicy{newFake(t)}, nil }
	if err := m.SetupFilter(); err != nil || m.IPv6Filtered {
		t.Errorf("a broken ip6tables with IPv6 off must start without IPv6: err=%v filtered=%v", err, m.IPv6Filtered)
	}
}

type failPolicy struct{ *fakeNetfilter }

func (f *failPolicy) ChangePolicy(_, _, _ string) error { return fmt.Errorf("no ip6tables table") }
