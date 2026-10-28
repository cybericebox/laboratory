//go:build linux

package gateway

import (
	"encoding/json"
	"os/exec"
	"testing"
	"time"

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

// Real traffic through a shaped lab interface: TCP between the pod and a lab, in both directions, comes out near the ceiling, and
// unshaped it is far above it. Needs iperf3 (the CI image of make test-netns has it).
func TestNetnsShaperHoldsTrafficToTheCeiling(t *testing.T) {
	nstest.Require(t)
	if _, err := exec.LookPath("iperf3"); err != nil {
		t.Skip("iperf3 is not installed")
	}
	nstest.NS(t, "lab")
	nstest.Veth(t, "", "lab1", "10.9.0.1/24", "lab", "l0", "10.9.0.2/24")
	srv := exec.Command("iperf3", "-s", "-B", "10.9.0.1", "-p", "5201")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _, _ = srv.Process.Wait() })
	time.Sleep(500 * time.Millisecond)

	measure := func(reverse bool) float64 {
		args := []string{"iperf3", "-c", "10.9.0.1", "-p", "5201", "-t", "6", "-O", "2", "-J"}
		if reverse {
			args = append(args, "-R")
		}
		out, err := nstest.Try("lab", args...)
		if err != nil {
			t.Fatalf("iperf3: %v\n%s", err, out)
		}
		var r struct {
			End struct {
				SumReceived struct {
					BitsPerSecond float64 `json:"bits_per_second"`
				} `json:"sum_received"`
			} `json:"end"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return r.End.SumReceived.BitsPerSecond / 1e6
	}
	free := measure(false)
	if free < 200 {
		t.Fatalf("control: an unshaped veth carries only %.0f Mbit/s", free)
	}
	s := NewShaper()
	if err := s.Track("lab1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLimit(20_000); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]float64{"lab to pod (policed)": measure(false), "pod to lab (shaped)": measure(true)} {
		if got < 12 || got > 22 {
			t.Errorf("%s: %.1f Mbit/s under a ceiling of 20", name, got)
		}
		t.Logf("%s: %.1f Mbit/s (unshaped %.0f)", name, got, free)
	}
	if err := s.SetLimit(60_000); err != nil {
		t.Fatal(err)
	}
	if got := measure(true); got < 40 || got > 66 {
		t.Errorf("after the ceiling went to 60: %.1f Mbit/s", got)
	}
}
