package dhcp

import (
	"net"
	"sync"
	"testing"
	"time"
)

var macA = net.HardwareAddr{0, 0, 0, 0, 0, 1}
var macB = net.HardwareAddr{0, 0, 0, 0, 0, 2}

func smallPool() *ipPool {
	_, s, _ := net.ParseCIDR("10.9.4.0/24")
	return newIPPool(s, net.ParseIP("10.9.4.1"), []Range{{2, 2}})
}
func TestExpiredLeaseAndOfferCanBeReused(t *testing.T) {
	for _, offer := range []bool{false, true} {
		p := smallPool()
		now := time.Unix(1000, 0)
		p.now = func() time.Time { return now }
		var ip net.IP
		var err error
		if offer {
			ip, err = p.Offer(macA)
		} else {
			ip, err = p.Allocate(macA)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.Allocate(macB); err == nil {
			t.Fatal("live reservation reused")
		}
		d := leaseDuration
		if offer {
			d = offerDuration
		}
		now = now.Add(d + time.Second)
		other, err := p.Allocate(macB)
		if err != nil || !other.Equal(ip) {
			t.Fatalf("reuse %v %v", other, err)
		}
	}
}
func TestLeaseReleaseOwnershipAndRenewal(t *testing.T) {
	p := smallPool()
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	ip, _ := p.Allocate(macA)
	if p.Release(macB, ip) {
		t.Fatal("foreign release")
	}
	now = now.Add(23 * time.Hour)
	if _, err := p.Commit(macA, ip); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := p.Allocate(macB); err == nil {
		t.Fatal("renewal expired early")
	}
	if !p.Release(macA, ip) {
		t.Fatal("owner release failed")
	}
	if _, err := p.Allocate(macB); err != nil {
		t.Fatal(err)
	}
}
func TestRangeUpdateReservesOldLeaseWithoutRenewal(t *testing.T) {
	p := smallPool()
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	ip, _ := p.Allocate(macA)
	if err := p.UpdateRanges([]Range{{3, 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commit(macA, ip); err == nil {
		t.Fatal("out-of-range lease extended")
	}
	other, err := p.Allocate(macB)
	if err != nil || other.Equal(ip) {
		t.Fatalf("range update %v %v", other, err)
	}
	if !p.Release(macA, ip) {
		t.Fatal("old active lease forgotten")
	}
}
func TestConcurrentLeasesAreUnique(t *testing.T) {
	_, s, _ := net.ParseCIDR("10.9.4.0/24")
	p := newIPPool(s, net.ParseIP("10.9.4.2"), []Range{{2, 254}})
	var wg sync.WaitGroup
	seen := sync.Map{}
	for i := 1; i <= 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip, err := p.Allocate(net.HardwareAddr{0, 0, 0, 0, 1, byte(i)})
			if err != nil {
				t.Error(err)
				return
			}
			if ip.Equal(p.gw) {
				t.Error("gateway allocated")
			}
			if _, old := seen.LoadOrStore(ip.String(), true); old {
				t.Error("duplicate lease")
			}
		}(i)
	}
	wg.Wait()
}

func TestRangeShrinkOffersNewAddressAndRetainsOldReservation(t *testing.T) {
	p := smallPool()
	old, err := p.Allocate(macA)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.UpdateRanges([]Range{{3, 3}})
	offered, err := p.Offer(macA)
	if err != nil || offered.Equal(old) {
		t.Fatalf("out-of-range offer %v %v", offered, err)
	}
	if _, err := p.Commit(macA, offered); err != nil {
		t.Fatal("migration commit", err)
	}
	_ = p.UpdateRanges([]Range{{2, 3}})
	if _, err := p.Allocate(macB); err == nil {
		t.Fatal("old live reservation recycled before expiry")
	}
}

func TestMigratedOwnerCanReleaseOldReservation(t *testing.T) {
	p := smallPool()
	old, _ := p.Allocate(macA)
	_ = p.UpdateRanges([]Range{{3, 3}})
	offered, err := p.Offer(macA)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Commit(macA, offered)
	if p.Release(macB, old) {
		t.Fatal("foreign retired lease release")
	}
	if !p.Release(macA, old) {
		t.Fatal("owner could not release old reservation")
	}
	_ = p.UpdateRanges([]Range{{2, 3}})
	reused, err := p.Allocate(macB)
	if err != nil || !reused.Equal(old) {
		t.Fatalf("released reservation not reusable %v %v", reused, err)
	}
}
