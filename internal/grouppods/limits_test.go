package grouppods

import "testing"

// The sizes at the owner's ceiling (10 clients, 50 labs) and at the defaults (1, 5), from the measured coefficients plus 50 %.
func TestGroupSizesFromTheMeasuredFormulas(t *testing.T) {
	v := VPNSize(10, 50)
	// memory: 9 + 6 + 5 + 8 = ~28 Mi measured-model, x1.5 = ~42 Mi -> 44 Mi; CPU: 5 + 3 + 5 + 60 = 73m x1.5 = 110m
	if v.Memory != 44<<20 || v.CPU < 105 || v.CPU > 115 {
		t.Errorf("VPN at 10 clients x 50 labs: %+v", v)
	}
	if v := VPNSize(1, 5); v.Memory != memFloorVPN || v.CPU != cpuFloor {
		t.Errorf("VPN at the defaults is the floor: %+v", v)
	}
	if g := GatewaySize(50); g.Memory != 24<<20 || g.CPU < 25 || g.CPU > 30 {
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
