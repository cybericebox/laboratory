//go:build linux

package vpn

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/podinput"
)

// stepTable is an inputTable that lets every step succeed except the one named by failOn.
type stepTable struct {
	failOn string
	rules  map[string][]string
	chains map[string]bool
}

var errStep = errors.New("step failed")

func newStepTable(failOn string) *stepTable {
	return &stepTable{failOn: failOn, rules: map[string][]string{}, chains: map[string]bool{}}
}
func (s *stepTable) step(name string) error {
	if s.failOn == name {
		return errStep
	}
	return nil
}
func (s *stepTable) ChainExists(_, c string) (bool, error) { return s.chains[c], s.step("ChainExists") }
func (s *stepTable) ClearChain(_, c string) error          { s.chains[c] = true; return s.step("ClearChain") }
func (s *stepTable) ClearAndDeleteChain(_, c string) error { delete(s.chains, c); return nil }
func (s *stepTable) Exists(_, c string, r ...string) (bool, error) {
	for _, x := range s.rules[c] {
		if x == strings.Join(r, " ") {
			return true, s.step("Exists")
		}
	}
	return false, s.step("Exists")
}
func (s *stepTable) Insert(_, c string, _ int, r ...string) error {
	s.rules[c] = append(s.rules[c], strings.Join(r, " "))
	return s.step("Insert")
}
func (s *stepTable) Append(_, c string, r ...string) error {
	s.rules[c] = append(s.rules[c], strings.Join(r, " "))
	return s.step("Append")
}
func (s *stepTable) Delete(_, _ string, _ ...string) error { return nil }
func (s *stepTable) ChangePolicy(_, _, _ string) error     { return s.step("ChangePolicy") }

// A security setup step that fails stops the start: protectInput returns the error, InitServer returns it, and the binary exits
// non-zero (internal/cmds/vpn). There is no log-and-continue.
func TestProtectInputEveryFailingStepStopsTheStart(t *testing.T) {
	policy := podinput.Policy{Uplink: "eth0", Probe: &podinput.Probe{Iface: "wg0", Addr: "10.8.0.1", Port: 8088}}
	ok := func() error { return nil }
	bad := func() error { return errStep }
	for _, step := range []string{"ChangePolicy", "ClearChain", "Exists", "Insert", "Append"} {
		if _, err := protectInput(newStepTable(step), nil, policy, ok, ok); err == nil {
			t.Errorf("IPv4 step %s failed and the setup went on", step)
		}
	}
	if _, err := protectInput(newStepTable(""), nil, policy, ok, bad); err == nil {
		t.Errorf("conntrack helpers that cannot be switched off must stop the start")
	}
	if _, err := protectInput(newStepTable(""), nil, policy, bad, ok); err == nil {
		t.Errorf("no ip6tables and IPv6 on must stop the start")
	}
	// an unusable ip6tables (every step fails) falls back to "IPv6 must be off", which is then an error too
	if _, err := protectInput(newStepTable(""), newStepTable("ChangePolicy"), policy, bad, ok); err == nil {
		t.Errorf("a broken ip6tables and IPv6 on must stop the start")
	}
	if f, err := protectInput(newStepTable(""), newStepTable(""), policy, bad, ok); err != nil || !f {
		t.Errorf("a working ip6tables: filtered=%v err=%v", f, err)
	}
}
