package grouppods

import "testing"

// The sizes of the formulas: base = the size a pod had before (VPN 100m / 320Mi, gateway 10m / 32Mi), the margin already inside the
// coefficients. Memory is rounded up to 4 Mi.
func TestGroupSizesFromTheFormulas(t *testing.T) {
	for _, c := range []struct {
		name     string
		got      Overhead
		cpu, mem int64
	}{
		{"vpn 1 user 5 labs", VPNSize(1, 5), 114, 328 << 20},      // 113.05m, 324.07 Mi
		{"vpn 10 users 50 labs", VPNSize(10, 50), 312, 372 << 20}, // 311.5m, 371.5 Mi
		{"gateway 5 labs", GatewaySize(5), 12, 36 << 20},          // 11.9m, 33.8 Mi
		{"gateway 50 labs", GatewaySize(50), 29, 52 << 20},        // 28.75m, 50.0 Mi
	} {
		if c.got.CPU != c.cpu || c.got.Memory != c.mem {
			t.Errorf("%s: %dm %dMi, want %dm %dMi", c.name, c.got.CPU, c.got.Memory>>20, c.cpu, c.mem>>20)
		}
	}
}

// A group is never smaller than a pod used to be, whatever it is made for (also with nothing but the base), and more load never means a
// smaller pod.
func TestGroupSizesAreNeverBelowTheOldDefaults(t *testing.T) {
	const oldVPNCPU, oldVPNMem, oldGWCPU, oldGWMem = 100, 320 << 20, 10, 32 << 20
	for u := 0; u <= 10; u++ {
		for l := 0; l <= 50; l += 5 {
			v, g := VPNSize(u, l), GatewaySize(l)
			if v.CPU < oldVPNCPU || v.Memory < oldVPNMem || g.CPU < oldGWCPU || g.Memory < oldGWMem {
				t.Fatalf("users %d labs %d below the old default: vpn %+v gateway %+v", u, l, v, g)
			}
			if u > 0 {
				if p := VPNSize(u-1, l); v.CPU < p.CPU || v.Memory < p.Memory {
					t.Fatalf("the VPN size fell with the users at %d,%d", u, l)
				}
			}
			if l >= 5 {
				if p := GatewaySize(l - 5); g.CPU < p.CPU || g.Memory < p.Memory {
					t.Fatalf("the gateway size fell with the labs at %d", l)
				}
			}
		}
	}
}
