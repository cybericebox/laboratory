package laboratory

import "testing"

func TestValidIfaceName(t *testing.T) {
	good := []string{"eth0", "eth1", "lab7", "gw254", "a", "e_th-0.1"}
	for _, n := range good {
		if _, ok := validIfaceName(n); !ok {
			t.Errorf("expected %q valid", n)
		}
	}
	bad := []string{"", "eth0; rm -rf /", "$(reboot)", "a b", "eth0\nrm", "toolonginterfacename", "-eth0", "a|b"}
	for _, n := range bad {
		if _, ok := validIfaceName(n); ok {
			t.Errorf("expected %q rejected", n)
		}
	}
}

func TestCanonicalCIDR(t *testing.T) {
	if got, ok := canonicalCIDR("10.10.1.10/24"); !ok || got != "10.10.1.10/24" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	// Injection attempts and malformed values must be rejected.
	for _, s := range []string{"10.0.0.1/24; reboot", "$(id)", "10.0.0.1", "notanip/24", ""} {
		if _, ok := canonicalCIDR(s); ok {
			t.Errorf("expected %q rejected", s)
		}
	}
}

func TestCanonicalIP(t *testing.T) {
	if got, ok := canonicalIP("10.10.1.1"); !ok || got != "10.10.1.1" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	for _, s := range []string{"10.0.0.1 && curl evil", "$(reboot)", "10.0.0.1/24", ""} {
		if _, ok := canonicalIP(s); ok {
			t.Errorf("expected %q rejected", s)
		}
	}
}
