package laboratory

import (
	"fmt"
	"testing"
)

func TestSubnetCIDRFormat(t *testing.T) {
	n := uint(5)
	vpnCIDR := fmt.Sprintf("10.%d.%d.0/24", vpnSubnetOctet2, n)
	if vpnCIDR != "10.8.5.0/24" {
		t.Fatalf("expected 10.8.5.0/24, got %s", vpnCIDR)
	}
	inetCIDR := fmt.Sprintf("10.%d.%d.0/24", inetSubnetOctet2, n)
	if inetCIDR != "10.9.5.0/24" {
		t.Fatalf("expected 10.9.5.0/24, got %s", inetCIDR)
	}
}
