package netutil

import "testing"

func TestSubnetCount(t *testing.T) {
	for base, want := range map[string]uint{
		"10.128.0.0/9":  32768,
		"10.128.0.0/10": 16384,
		"10.0.0.0/8":    65536,
		"10.8.0.0/10":   16384 - 8*256, // literal start: 10.8.0.0 to the end of 10.0.0.0/10
	} {
		got, err := SubnetCount(base, 24)
		if err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", base, got, err, want)
		}
	}
	if _, err := SubnetCount("10.0.0.0/25", 24); err == nil {
		t.Error("a base smaller than a child must be refused")
	}
	if _, err := SubnetCount("nonsense", 24); err == nil {
		t.Error("garbage must be refused")
	}
}

func TestSubnetIndexInvertsSubnetForIndex(t *testing.T) {
	for _, base := range []string{"10.128.0.0/9", "10.8.0.0/10"} {
		for _, n := range []uint{0, 1, 254, 255, 256, 16383} {
			cidr, err := SubnetForIndex(base, 24, n)
			if err != nil {
				continue
			}
			got, err := SubnetIndex(base, 24, cidr)
			if err != nil || got != n {
				t.Errorf("%s index %d (%s): %d, %v", base, n, cidr, got, err)
			}
		}
	}
	// The Internet /24 of a lab made before the one pool is position 16384+N of the /9.
	if n, err := SubnetIndex("10.128.0.0/9", 24, "10.192.7.0/24"); err != nil || n != 16384+7 {
		t.Errorf("old internet /24: %d, %v", n, err)
	}
	for _, bad := range []string{"10.128.0.1/24", "10.128.0.0/25", "10.7.0.0/24", "192.168.0.0/24", "x"} {
		if _, err := SubnetIndex("10.128.0.0/9", 24, bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
