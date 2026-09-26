package vpnprobe

import "testing"

func TestGatewayIPDerivesFirstHostFromClientSubnet(t *testing.T) {
	ip, err := GatewayIP("10.8.7.0/24")
	if err != nil || ip != "10.8.7.1" {
		t.Fatalf("gateway=%q err=%v", ip, err)
	}
	for _, invalid := range []string{"", "10.8.7.1/32", "2001:db8::/64", "not-a-subnet"} {
		if _, err := GatewayIP(invalid); err == nil {
			t.Fatalf("accepted invalid subnet %q", invalid)
		}
	}
}
