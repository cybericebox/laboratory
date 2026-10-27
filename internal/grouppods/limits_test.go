package grouppods

import "testing"

// The sizes at the owner's ceiling (10 clients, 50 labs) and at the defaults (1, 5), from the measured coefficients plus 50 %.
func TestGroupSizesFromTheMeasuredFormulas(t *testing.T) {
	v := VPNSize(10, 50)
	// memory: 9 + 21 + 5 + 8 = ~43 Mi (live traffic peak 42 Mi), x1.5 = ~64 Mi -> 68 Mi; CPU: 5 + 59 + 5 + 60 = ~129m (live peak 129m) x1.5 = 194m
	if v.Memory != 68<<20 || v.CPU < 190 || v.CPU > 200 {
		t.Errorf("VPN at 10 clients x 50 labs: %+v", v)
	}
	if v := VPNSize(1, 5); v.Memory != memFloorVPN || v.CPU < cpuFloor || v.CPU > 20 {
		t.Errorf("VPN at the defaults is the memory floor: %+v", v)
	}
	if g := GatewaySize(50); g.Memory != 32<<20 || g.CPU < 25 || g.CPU > 30 {
		t.Errorf("gateway at 50 labs: %+v", g)
	}
	if g := GatewaySize(5); g.Memory != memFloorGateway || g.CPU != cpuFloor {
		t.Errorf("gateway at the defaults is the floor: %+v", g)
	}
	// more load never means a smaller pod
	prev := Overhead{}
	for u := 0; u <= 10; u++ {
		for l := 0; l <= 50; l += 10 {
			got := VPNSize(u, l)
			if u > 0 && l > 0 && (got.CPU < VPNSize(u-1, l).CPU || got.Memory < VPNSize(u-1, l).Memory) {
				t.Fatalf("VPN size fell with the users at %d,%d: %+v", u, l, got)
			}
			prev = got
		}
	}
	_ = prev
}
