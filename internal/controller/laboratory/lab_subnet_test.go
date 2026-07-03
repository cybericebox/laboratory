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
