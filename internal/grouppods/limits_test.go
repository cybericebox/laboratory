package grouppods

import "testing"

// The sizes of the formulas (see limits.go for the measurements). Memory is rounded up to 4 Mi.
func TestGroupSizesFromTheFormulas(t *testing.T) {
	for _, c := range []struct {
		name     string
		got      Overhead
		cpu, mem int64
	}{
		{"vpn 1 user 5 labs", VPNSize(1, 5), 26, 168 << 20},       // 25.3m, 167.9 Mi
		{"vpn 5 users 13 labs", VPNSize(5, 13), 83, 216 << 20},    // 82.3m, 213.2 Mi
		{"vpn 10 users 50 labs", VPNSize(10, 50), 224, 288 << 20}, // 223.5m, 285.5 Mi
		{"gateway 5 labs", GatewaySize(5), 12, 36 << 20},          // 11.9m, 33.8 Mi
		{"gateway 50 labs", GatewaySize(50), 29, 52 << 20},        // 28.75m, 50.0 Mi
	} {
		if c.got.CPU != c.cpu || c.got.Memory != c.mem {
			t.Errorf("%s: %dm %dMi, want %dm %dMi", c.name, c.got.CPU, c.got.Memory>>20, c.cpu, c.mem>>20)
		}
	}
}

// The formulas keep a margin of at least 35 % over every measured point of the VPN pod: kind (10 clients, 50 labs, 500 rules: 146m,
// 43 Mi), the older local stand (10 active clients: 55m, 195 Mi) and AWS (20 connected clients under load: 6m, 25 Mi).
func TestVPNSizeKeepsTheMarginOverEveryMeasurement(t *testing.T) {
	for _, m := range []struct {
		what        string
		users, labs int
		cpu, memMiB float64
	}{
		{"kind 10 clients, 50 labs, 500 rules", 10, 50, 146, 43},
		{"local stand 10 active clients", 10, 0, 55, 195},
		{"local stand 5 active clients", 5, 0, 48, 155},
		{"AWS 20 connected clients under load", 20, 0, 6, 25},
		{"AWS empty group", 0, 0, 1.2, 10},
	} {
		v := VPNSize(m.users, m.labs)
		if float64(v.CPU) < m.cpu*1.35 || float64(v.Memory>>20) < m.memMiB*1.35 {
			t.Errorf("%s: sized %dm %dMi, measured %.0fm %.0fMi: under +35%%", m.what, v.CPU, v.Memory>>20, m.cpu, m.memMiB)
		}
	}
}

// More load never means a smaller pod.
func TestGroupSizesNeverFallWithTheLoad(t *testing.T) {
	for u := 0; u <= 10; u++ {
		for l := 0; l <= 50; l += 5 {
			v, g := VPNSize(u, l), GatewaySize(l)
			if u > 0 {
				if p := VPNSize(u-1, l); v.CPU < p.CPU || v.Memory < p.Memory {
					t.Fatalf("the VPN size fell with the users at %d,%d", u, l)
				}
			}
			if l >= 5 {
				if p := GatewaySize(l - 5); g.CPU < p.CPU || g.Memory < p.Memory {
					t.Fatalf("the gateway size fell with the labs at %d", l)
				}
				if p := VPNSize(u, l-5); v.CPU < p.CPU || v.Memory < p.Memory {
					t.Fatalf("the VPN size fell with the labs at %d,%d", u, l)
				}
			}
		}
	}
}
