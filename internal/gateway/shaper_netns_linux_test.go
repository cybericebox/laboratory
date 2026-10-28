//go:build linux

package gateway

import (
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/cybericebox/laboratory/internal/nstest"
)

// The shaper on a real lab interface in a network namespace: the egress is a token bucket filter at the ceiling, the ingress has a
// police, a change of the ceiling changes both, and Forget leaves the interface bare. (Run by make test-netns.)
func TestNetnsShaperKeepsALabInterfaceToItsCeiling(t *testing.T) {
	nstest.Require(t)
	nstest.NS(t, "lab")
	nstest.Veth(t, "", "lab1", "10.9.0.1/24", "lab", "l0", "10.9.0.2/24")
	s := NewShaper()

	state := func() (rate uint64, hasIngress, hasPolice bool) {
		link, err := netlink.LinkByName("lab1")
		if err != nil {
			t.Fatal(err)
		}
		qdiscs, err := netlink.QdiscList(link)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range qdiscs {
			switch q := q.(type) {
			case *netlink.Tbf:
				rate = q.Rate
			case *netlink.Ingress:
				hasIngress = true
			}
		}
		filters, err := netlink.FilterList(link, netlink.MakeHandle(0xffff, 0))
		if err != nil {
			t.Fatal(err)
		}
		hasPolice = len(filters) > 0
		return
	}

	if err := s.Track("lab1"); err != nil {
		t.Fatal(err)
	}
	if rate, ing, pol := state(); rate != 0 || ing || pol {
		t.Fatalf("no ceiling yet, the interface stays bare: rate %d ingress %v police %v", rate, ing, pol)
	}
	if err := s.SetLimit(50_000); err != nil {
		t.Fatal(err)
	}
	if rate, ing, pol := state(); rate != 6_250_000 || !ing || !pol {
		t.Fatalf("50 Mbit/s: rate %d ingress %v police %v", rate, ing, pol)
	}
	if err := s.SetLimit(20_000); err != nil {
		t.Fatal(err)
	}
	if rate, _, _ := state(); rate != 2_500_000 {
		t.Fatalf("20 Mbit/s: rate %d", rate)
	}
	if err := s.SetLimit(20_000); err != nil {
		t.Fatalf("the same ceiling again must be a no-op: %v", err)
	}
	if err := s.SetLimit(0); err != nil {
		t.Fatal(err)
	}
	if rate, ing, pol := state(); rate != 0 || ing || pol {
		t.Fatalf("ceiling off: rate %d ingress %v police %v", rate, ing, pol)
	}
	if err := s.SetLimit(10_000); err != nil {
		t.Fatal(err)
	}
	s.Forget("lab1")
	if rate, ing, pol := state(); rate != 0 || ing || pol {
		t.Fatalf("forgotten: rate %d ingress %v police %v", rate, ing, pol)
	}
}
