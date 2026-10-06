package podinput

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeFilter keeps chains as ordered rule specs and evaluates a packet through INPUT like netfilter does (first match decides, the
// policy is ACCEPT as in a pod). After every change it checks that once INPUT jumps to the policy chain, the chain ends in the
// drop and never lets a lab packet through.
type fakeFilter struct {
	t      *testing.T
	chains map[string][][]string
}

func newFake(t *testing.T) *fakeFilter {
	return &fakeFilter{t: t, chains: map[string][][]string{"INPUT": nil}}
}

func (f *fakeFilter) ChainExists(_, chain string) (bool, error) {
	_, ok := f.chains[chain]
	return ok, nil
}
func (f *fakeFilter) ClearChain(_, chain string) error {
	f.chains[chain] = nil
	f.check()
	return nil
}
func (f *fakeFilter) ClearAndDeleteChain(_, chain string) error {
	delete(f.chains, chain)
	return nil
}
func (f *fakeFilter) idx(chain string, rule []string) int {
	for i, r := range f.chains[chain] {
		if strings.Join(r, " ") == strings.Join(rule, " ") {
			return i
		}
	}
	return -1
}
func (f *fakeFilter) Exists(_, chain string, rule ...string) (bool, error) {
	return f.idx(chain, rule) >= 0, nil
}
func (f *fakeFilter) Insert(_, chain string, pos int, rule ...string) error {
	rs := append([][]string{}, f.chains[chain][:pos-1]...)
	rs = append(rs, append([]string{}, rule...))
	f.chains[chain] = append(rs, f.chains[chain][pos-1:]...)
	f.check()
	return nil
}
func (f *fakeFilter) Append(_, chain string, rule ...string) error {
	f.chains[chain] = append(f.chains[chain], append([]string{}, rule...))
	f.check()
	return nil
}
func (f *fakeFilter) Delete(_, chain string, rule ...string) error {
	if i := f.idx(chain, rule); i >= 0 {
		f.chains[chain] = append(append([][]string{}, f.chains[chain][:i]...), f.chains[chain][i+1:]...)
		f.check()
	}
	return nil
}

// pkt is a packet addressed to the pod.
type pkt struct {
	iface string
	dst   string
	proto string // tcp, udp, icmp, icmpv6
	dport int
	icmp6 string // icmpv6 type name
	icmp4 string // icmp type name
	state string // NEW or ESTABLISHED
}

func ifaceMatch(pattern, iface string) bool {
	if strings.HasSuffix(pattern, "+") {
		return strings.HasPrefix(iface, strings.TrimSuffix(pattern, "+"))
	}
	return pattern == iface
}

func (f *fakeFilter) matches(r []string, p pkt) (match bool, target string) {
	match = true
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case "-i":
			match = match && ifaceMatch(r[i+1], p.iface)
			i++
		case "-d":
			match = match && r[i+1] == p.dst
			i++
		case "-p":
			match = match && r[i+1] == p.proto
			i++
		case "--dport":
			match = match && r[i+1] == strconv.Itoa(p.dport)
			i++
		case "--icmp-type":
			match = match && r[i+1] == p.icmp4
			i++
		case "--icmpv6-type":
			match = match && r[i+1] == p.icmp6
			i++
		case "--ctstate":
			ok := false
			for _, s := range strings.Split(r[i+1], ",") {
				ok = ok || s == p.state
			}
			match = match && ok
			i++
		case "-m":
			i++
		case "-j":
			target = r[i+1]
			i++
		}
	}
	return match, target
}

func (f *fakeFilter) eval(chain string, p pkt) string {
	for _, r := range f.chains[chain] {
		if m, target := f.matches(r, p); m {
			switch target {
			case "ACCEPT", "DROP":
				return target
			default:
				if v := f.eval(target, p); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

func (f *fakeFilter) verdict(p pkt) string {
	if p.state == "" {
		p.state = "NEW"
	}
	if v := f.eval("INPUT", p); v != "" {
		return v
	}
	return "ACCEPT"
}

// check: as soon as INPUT jumps to the policy chain, the chain ends in the drop, and a new lab packet is never accepted.
func (f *fakeFilter) check() {
	f.t.Helper()
	if f.idx("INPUT", []string{"-j", Chain}) < 0 {
		return
	}
	rules := f.chains[Chain]
	if len(rules) == 0 || strings.Join(rules[len(rules)-1], " ") != "-j DROP" {
		f.t.Fatalf("INPUT jumps to %s, which does not end in the drop: %v", Chain, rules)
	}
	for _, iface := range []string{"lab1", "lab7", "br0"} {
		for _, pr := range []string{"tcp", "udp", "icmp"} {
			if v := f.verdict(pkt{iface: iface, dst: "10.8.0.1", proto: pr, dport: 22}); v != "DROP" {
				f.t.Fatalf("a %s packet from %s is %s while the policy stands: %v", pr, iface, v, rules)
			}
		}
	}
}

var vpnPolicy = Policy{Uplink: "eth0", Probe: &Probe{Iface: "wg0", Addr: "10.8.0.1", Port: 8088}}

func TestLabSideGetsNoAnswerFromThePod(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, false); err != nil {
		t.Fatal(err)
	}
	for _, iface := range []string{"lab1", "lab42", "br0", "veth9", "tun0"} {
		for _, dst := range []string{"10.8.0.1", "10.8.100.1", "10.99.0.5"} {
			for _, p := range []pkt{
				{proto: "tcp", dport: 8088}, {proto: "tcp", dport: 22}, {proto: "udp", dport: 51820}, {proto: "udp", dport: 53},
				{proto: "udp", dport: 67}, {proto: "icmp"},
			} {
				p.iface, p.dst = iface, dst
				if v := f.verdict(p); v != "DROP" {
					t.Errorf("%s -> %s %s/%d: %s, want DROP", iface, dst, p.proto, p.dport, v)
				}
			}
		}
	}
}

func TestParticipantsReachOnlyTheStatusPage(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, false); err != nil {
		t.Fatal(err)
	}
	if v := f.verdict(pkt{iface: "wg0", dst: "10.8.0.1", proto: "tcp", dport: 8088}); v != "ACCEPT" {
		t.Errorf("the status page from the tunnel: %s, want ACCEPT", v)
	}
	for _, p := range []pkt{
		{iface: "wg0", dst: "10.8.0.1", proto: "tcp", dport: 22},
		{iface: "wg0", dst: "10.8.0.1", proto: "tcp", dport: 8089},
		{iface: "wg0", dst: "10.8.0.1", proto: "udp", dport: 8088},
		{iface: "wg0", dst: "10.8.0.1", proto: "udp", dport: 51820},
		{iface: "wg0", dst: "10.8.0.1", proto: "icmp"},
		{iface: "wg0", dst: "10.8.100.1", proto: "tcp", dport: 8088}, // the lab-side address of the pod
		{iface: "lab1", dst: "10.8.0.1", proto: "tcp", dport: 8088},  // the page through a lab interface
	} {
		if v := f.verdict(p); v != "DROP" {
			t.Errorf("%+v: %s, want DROP", p, v)
		}
	}
}

func TestUplinkLoopbackAndRepliesStayOpen(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []pkt{
		{iface: "eth0", dst: "10.244.1.5", proto: "udp", dport: 51820},
		{iface: "eth0", dst: "10.244.1.5", proto: "tcp", dport: 8081}, // a kubelet probe
		{iface: "eth0", dst: "10.244.1.5", proto: "icmp"},
		{iface: "lo", dst: "127.0.0.1", proto: "tcp", dport: 9000},
		{iface: "lab1", dst: "10.8.100.1", proto: "tcp", dport: 40000, state: "ESTABLISHED"}, // a reply to the pod's own connection
		{iface: "lab1", dst: "10.8.100.1", proto: "icmp", state: "RELATED"},
	} {
		if v := f.verdict(p); v != "ACCEPT" {
			t.Errorf("%+v: %s, want ACCEPT", p, v)
		}
	}
}

func TestDHCPOnlyOnTheInterfacesThatRunIt(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, false); err != nil {
		t.Fatal(err)
	}
	dhcp := func(iface string) string {
		return f.verdict(pkt{iface: iface, dst: "255.255.255.255", proto: "udp", dport: 67})
	}
	if dhcp("lab1") != "DROP" {
		t.Fatalf("DHCP is open before it is asked for")
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := AllowDHCP(f, "lab1"); err != nil {
			t.Fatal(err)
		}
	}
	if dhcp("lab1") != "ACCEPT" || dhcp("lab2") != "DROP" || dhcp("wg0") != "DROP" {
		t.Errorf("DHCP must be open on lab1 only: lab1=%s lab2=%s wg0=%s", dhcp("lab1"), dhcp("lab2"), dhcp("wg0"))
	}
	if v := f.verdict(pkt{iface: "lab1", dst: "10.8.100.1", proto: "udp", dport: 53}); v != "DROP" {
		t.Errorf("DNS to the pod on lab1: %s, want DROP", v)
	}
	DenyDHCP(f, "lab1")
	if dhcp("lab1") != "DROP" {
		t.Errorf("DHCP still open on lab1 after DenyDHCP")
	}
}

func TestIPv6KeepsNeighbourDiscoveryOnly(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, true); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []string{"neighbour-solicitation", "neighbour-advertisement"} {
		if v := f.verdict(pkt{iface: "lab1", dst: "fe80::1", proto: "icmpv6", icmp6: ty}); v != "ACCEPT" {
			t.Errorf("%s from lab1: %s, want ACCEPT", ty, v)
		}
	}
	for _, p := range []pkt{
		{iface: "lab1", dst: "fe80::1", proto: "icmpv6", icmp6: "echo-request"},
		{iface: "lab1", dst: "fe80::1", proto: "tcp", dport: 22},
		{iface: "wg0", dst: "fe80::1", proto: "icmpv6", icmp6: "echo-request"},
		{iface: "wg0", dst: "fd00::1", proto: "tcp", dport: 8088},
	} {
		if v := f.verdict(p); v != "DROP" {
			t.Errorf("%+v: %s, want DROP", p, v)
		}
	}
	if v := f.verdict(pkt{iface: "eth0", dst: "fd00::1", proto: "tcp", dport: 8081}); v != "ACCEPT" {
		t.Errorf("the uplink over IPv6: %s, want ACCEPT", v)
	}
}

func TestGatewayPodHasNoProbe(t *testing.T) {
	f := newFake(t)
	if err := Install(f, Policy{Uplink: "eth0"}, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []pkt{
		{iface: "lab1", dst: "10.9.0.1", proto: "icmp"},
		{iface: "lab1", dst: "10.9.0.1", proto: "tcp", dport: 8088},
	} {
		if v := f.verdict(p); v != "DROP" {
			t.Errorf("%+v: %s, want DROP", p, v)
		}
	}
	if v := f.verdict(pkt{iface: "eth0", dst: "10.244.1.5", proto: "tcp", dport: 80}); v != "ACCEPT" {
		t.Errorf("the uplink: %s, want ACCEPT", v)
	}
}

func TestInstallIsIdempotentAndKeepsTheOrder(t *testing.T) {
	f := newFake(t)
	for i := 0; i < 3; i++ {
		if err := Install(f, vpnPolicy, false); err != nil {
			t.Fatal(err)
		}
	}
	want := Rules(vpnPolicy, false)
	got := f.chains[Chain]
	if len(got) != len(want)+1 {
		t.Fatalf("chain after three installs: %v, want %d accepts and the drop", got, len(want))
	}
	for i, r := range want {
		if strings.Join(got[i], " ") != strings.Join(r, " ") {
			t.Errorf("rule %d: %v, want %v", i, got[i], r)
		}
	}
	if n := len(f.chains["INPUT"]); n != 1 {
		t.Errorf("INPUT has %d rules, want the one jump", n)
	}
}

func TestRemoveTakesEverythingOff(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, false); err != nil {
		t.Fatal(err)
	}
	if err := AllowDHCP(f, "lab1"); err != nil {
		t.Fatal(err)
	}
	Remove(f)
	if len(f.chains["INPUT"]) != 0 {
		t.Errorf("INPUT after Remove: %v", f.chains["INPUT"])
	}
	if _, ok := f.chains[Chain]; ok {
		t.Errorf("chain %s survived Remove", Chain)
	}
}

func TestRulesAreTheExpectedSpecs(t *testing.T) {
	var got []string
	for _, r := range Rules(vpnPolicy, false) {
		got = append(got, strings.Join(r, " "))
	}
	want := []string{
		"-i lo -j ACCEPT",
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"-i eth0 -j ACCEPT",
		"-i wg0 -d 10.8.0.1 -p tcp --dport 8088 -j ACCEPT",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rules\n%v\nwant\n%v", got, want)
	}
	if got, want := strings.Join(DHCPRule("lab3"), " "), "-i lab3 -p udp --dport 67 -j ACCEPT"; got != want {
		t.Errorf("DHCP rule %q, want %q", got, want)
	}
}

func TestPingOnlyTheOwnAddressOfTheInterface(t *testing.T) {
	f := newFake(t)
	if err := Install(f, vpnPolicy, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := AllowPing(f, "lab1", "10.8.100.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := AllowPing(f, "lab2", "10.8.101.1"); err != nil {
		t.Fatal(err)
	}
	echo := func(iface, dst string) string {
		return f.verdict(pkt{iface: iface, dst: dst, proto: "icmp", icmp4: "echo-request"})
	}
	if echo("lab1", "10.8.100.1") != "ACCEPT" || echo("lab2", "10.8.101.1") != "ACCEPT" {
		t.Errorf("a lab must be able to ping the pod's address on its own interface")
	}
	for _, c := range []struct{ iface, dst, why string }{
		{"lab1", "10.8.101.1", "the address of another lab's interface"},
		{"lab2", "10.8.100.1", "the address of another lab's interface"},
		{"lab1", "10.8.0.1", "the tunnel address"},
		{"lab1", "10.244.0.2", "the uplink address"},
		{"lab1", "127.0.0.1", "the loopback"},
		{"lab3", "10.8.100.1", "an interface that has no rule"},
		{"wg0", "10.8.100.1", "the tunnel interface"},
		{"wg0", "10.8.0.1", "the tunnel interface, its own address"},
	} {
		if v := echo(c.iface, c.dst); v != "DROP" {
			t.Errorf("echo on %s to %s (%s): %s, want DROP", c.iface, c.dst, c.why, v)
		}
	}
	for _, p := range []pkt{
		{iface: "lab1", dst: "10.8.100.1", proto: "icmp", icmp4: "timestamp-request"},
		{iface: "lab1", dst: "10.8.100.1", proto: "icmp", icmp4: "redirect"},
		{iface: "lab1", dst: "10.8.100.1", proto: "tcp", dport: 22},
		{iface: "lab1", dst: "10.8.100.1", proto: "udp", dport: 53},
	} {
		if v := f.verdict(p); v != "DROP" {
			t.Errorf("%+v: %s, want DROP", p, v)
		}
	}
	DenyPing(f, "lab1", "10.8.100.1")
	if echo("lab1", "10.8.100.1") != "DROP" || echo("lab2", "10.8.101.1") != "ACCEPT" {
		t.Errorf("DenyPing must close lab1 only")
	}
	// the order: the limited accept, then the drop of the rest, both in front of the ESTABLISHED accept
	rules := f.chains[Chain]
	if len(rules) < 3 || !strings.Contains(strings.Join(rules[0], " "), "-m limit") || strings.Join(rules[1], " ") != strings.Join(pingOverLimitRule("lab2", "10.8.101.1"), " ") {
		t.Errorf("chain starts with %v", rules[:min(3, len(rules))])
	}
	got := strings.Join(PingRule("lab1", "10.8.100.1"), " ")
	if want := "-i lab1 -d 10.8.100.1 -p icmp --icmp-type echo-request -m limit --limit 10/second --limit-burst 20 -j ACCEPT"; got != want {
		t.Errorf("ping rule %q, want %q", got, want)
	}
}

func TestNoConntrackHelpers(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "none")
	if err := NoConntrackHelpers(missing); err != nil {
		t.Errorf("a missing switch (conntrack not loaded) is not an error: %v", err)
	}
	on := filepath.Join(dir, "on")
	if err := os.WriteFile(on, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NoConntrackHelpers(on); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(on); strings.TrimSpace(string(b)) != "0" {
		t.Errorf("helpers left on: %q", b)
	}
}

// failTable is a Table whose chosen step fails; it records nothing else.
type failTable struct {
	*fakeFilter
	failPolicy bool
}

func (f failTable) ChangePolicy(_, _, _ string) error {
	if f.failPolicy {
		return os.ErrPermission
	}
	return nil
}

func TestEnsureOff(t *testing.T) {
	dir := t.TempDir()
	on := filepath.Join(dir, "on")
	off := filepath.Join(dir, "off")
	_ = os.WriteFile(on, []byte("0\n"), 0o644)
	_ = os.WriteFile(off, []byte("1\n"), 0o644)
	if err := EnsureOff([]string{filepath.Join(dir, "absent")}); err != nil {
		t.Errorf("no IPv6 stack is off: %v", err)
	}
	if err := EnsureOff([]string{off, on}); err != nil {
		t.Errorf("a switch that can be turned: %v", err)
	}
	if b, _ := os.ReadFile(on); strings.TrimSpace(string(b)) != "1" {
		t.Errorf("not switched: %q", b)
	}
	// /dev/null accepts the write and reads back empty: a switch that cannot be turned (a read-only /proc/sys).
	if err := EnsureOff([]string{"/dev/null"}); err == nil {
		t.Errorf("IPv6 that cannot be switched off must be an error")
	}
}

func TestProtectIPv6FailsClosed(t *testing.T) {
	off := func(err error) func() error { return func() error { return err } }
	ok, bad := off(nil), off(os.ErrPermission)
	if f, err := ProtectIPv6(nil, vpnPolicy, ok); err != nil || f {
		t.Errorf("no ip6tables, IPv6 off: filtered=%v err=%v, want false, nil", f, err)
	}
	if _, err := ProtectIPv6(nil, vpnPolicy, bad); err == nil {
		t.Errorf("no ip6tables and IPv6 cannot be switched off must be an error")
	}
	if f, err := ProtectIPv6(failTable{fakeFilter: newFake(t)}, vpnPolicy, bad); err != nil || !f {
		t.Errorf("a working ip6tables needs no switch: filtered=%v err=%v", f, err)
	}
	if _, err := ProtectIPv6(failTable{fakeFilter: newFake(t), failPolicy: true}, vpnPolicy, bad); err == nil {
		t.Errorf("an unusable ip6tables and IPv6 on must be an error")
	}
	if f, err := ProtectIPv6(failTable{fakeFilter: newFake(t), failPolicy: true}, vpnPolicy, ok); err != nil || f {
		t.Errorf("an unusable ip6tables with IPv6 off is fine: filtered=%v err=%v", f, err)
	}
}
