package laboratory

import (
	"testing"
	
	"github.com/cybericebox/laboratory/pkg/netutil"
)

func TestSubnetCIDRFormat(t *testing.T) {
	tests := []struct {
		base string
		n    uint
		want string
	}{
		{"10.8.0.0/10", 5, "10.8.5.0/24"},
		{"10.9.0.0/10", 5, "10.9.5.0/24"},
		{"10.8.0.0/10", 0, "10.8.0.0/24"},
		{"10.8.0.0/10", 254, "10.8.254.0/24"},
		{"10.8.0.0/10", 256, "10.9.0.0/24"}, // 3rd octet rolls over, still inside /10
	}
	for _, tt := range tests {
		got, err := netutil.SubnetForIndex(tt.base, 24, tt.n)
		if err != nil {
			t.Fatalf("SubnetForIndex(%s, 24, %d): %v", tt.base, tt.n, err)
		}
		if got != tt.want {
			t.Errorf("SubnetForIndex(%s, 24, %d) = %s, want %s", tt.base, tt.n, got, tt.want)
		}
	}
}

func TestSubnetForIndexEscapesBase(t *testing.T) {
	// 10.8.0.0/10 spans 10.0.0.0–10.63.255.255; from literal 10.8.0.0 there is
	// room for 14336 /24 children before escaping the /10.
	if _, err := netutil.SubnetForIndex("10.8.0.0/10", 24, 14336); err == nil {
		t.Error("expected error for index escaping base network, got nil")
	}
	if _, err := netutil.SubnetForIndex("10.8.0.0/10", 24, 14335); err != nil {
		t.Errorf("last in-range index must succeed: %v", err)
	}
}

func TestVPNInetHalvesNonOverlapping(t *testing.T) {
	// Global 10.128.0.0/9 split into VPN 10.128.0.0/10 and internet 10.192.0.0/10.
	// For every lab index N the two /24 subnets must differ.
	for _, n := range []uint{0, 1, 5, 254, 255, 4095} {
		vpn, err := netutil.SubnetForIndex("10.128.0.0/10", 24, n)
		if err != nil {
			t.Fatalf("vpn N=%d: %v", n, err)
		}
		inet, err := netutil.SubnetForIndex("10.192.0.0/10", 24, n)
		if err != nil {
			t.Fatalf("inet N=%d: %v", n, err)
		}
		if vpn == inet {
			t.Errorf("N=%d: vpn and inet subnets overlap: %s", n, vpn)
		}
	}
	// First VPN /24 is the client subnet.
	if got, _ := netutil.SubnetForIndex("10.128.0.0/10", 24, 0); got != "10.128.0.0/24" {
		t.Errorf("client subnet = %s, want 10.128.0.0/24", got)
	}
}
