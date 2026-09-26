package dhcp

import (
	"net"
	"testing"
)

func TestIPPoolUsesConfiguredRanges(t *testing.T) {
	_, subnet, err := net.ParseCIDR("10.9.4.0/24")
	if err != nil {
		t.Fatal(err)
	}
	pool := newIPPool(subnet, net.ParseIP("10.9.4.1"), []Range{
		{Start: 3, End: 3}, {Start: 5, End: 6},
	})
	for _, want := range []string{"10.9.4.3", "10.9.4.5", "10.9.4.6"} {
		got, err := pool.Allocate(net.HardwareAddr{0, 0, 0, 0, 0, byte(len(pool.byMAC) + 1)})
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != want {
			t.Fatalf("allocated %s, want %s", got, want)
		}
	}
	if _, err := pool.Allocate(net.HardwareAddr{0, 0, 0, 0, 0, 9}); err == nil {
		t.Fatal("pool allocated outside the configured ranges")
	}
}

func TestIPPoolStickyLease(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.9.4.0/24")
	pool := newIPPool(subnet, net.ParseIP("10.9.4.1"), []Range{{Start: 2, End: 254}})
	mac := net.HardwareAddr{0, 0, 0, 0, 0, 1}
	first, err := pool.Allocate(mac)
	if err != nil || first.String() != "10.9.4.2" {
		t.Fatalf("first lease = %v, %v", first, err)
	}
	again, err := pool.Allocate(mac)
	if err != nil || !again.Equal(first) {
		t.Fatalf("sticky lease changed = %v, %v", again, err)
	}
}

func TestValidateRanges(t *testing.T) {
	for _, ranges := range [][]Range{
		{},
		{{Start: 1, End: 10}}, {{Start: 10, End: 255}},
		{{Start: 20, End: 10}}, {{Start: 2, End: 10}, {Start: 10, End: 20}},
	} {
		if err := ValidateRanges(ranges); err == nil {
			t.Fatalf("invalid ranges accepted: %+v", ranges)
		}
	}
	if err := ValidateRanges([]Range{{Start: 2, End: 10}, {Start: 20, End: 254}}); err != nil {
		t.Fatalf("valid ranges rejected: %v", err)
	}
}
