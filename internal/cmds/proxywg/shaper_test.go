package proxywg

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

type fakePath struct {
	limiter           bool
	active            int
	rate              float64
	burst             int
	session, class    int64
	rates, bandwidths int
}

func (f *fakePath) ByteLimiter() bool { return f.limiter }
func (f *fakePath) SetRate(r float64, b int) error {
	f.rate, f.burst = r, b
	f.rates++
	return nil
}
func (f *fakePath) SetBandwidth(s, c int64) error {
	f.session, f.class = s, c
	f.bandwidths++
	return nil
}
func (f *fakePath) ActiveSessions(time.Duration) (int, error) { return f.active, nil }

func newShaper(f *fakePath) *fastShaper {
	return &fastShaper{fp: f, base: 0, burst: 0, log: logr.Discard()}
}

func TestShaperGivesEachActiveSessionItsFairShare(t *testing.T) {
	f := &fakePath{limiter: true, active: 10}
	s := newShaper(f)
	s.set(bandwidth.Limits{ClassKbps: 340_000, CapKbps: 50_000})
	if f.session != 34_000 || f.class != 340_000 {
		t.Fatalf("ten sessions share the class: %d of %d", f.session, f.class)
	}
	f.active = 2
	s.refresh()
	if f.session != 50_000 {
		t.Fatalf("two sessions are held to the cap: %d", f.session)
	}
}

func TestShaperWritesOnlyWhenSomethingChanged(t *testing.T) {
	f := &fakePath{limiter: true, active: 5}
	s := newShaper(f)
	s.set(bandwidth.Limits{ClassKbps: 100_000, CapKbps: 50_000})
	n := f.bandwidths
	for i := 0; i < 5; i++ {
		s.refresh()
	}
	if f.bandwidths != n {
		t.Fatalf("the kernel was written %d times for nothing", f.bandwidths-n)
	}
}

func TestShaperFallsBackToThePacketRate(t *testing.T) {
	f := &fakePath{limiter: false}
	s := newShaper(f)
	s.set(bandwidth.Limits{ClassKbps: 340_000, CapKbps: 50_000})
	if f.bandwidths != 0 {
		t.Fatal("without the byte limiter the kernel gets no byte limits")
	}
	if f.rate < 4166 || f.rate > 4167 {
		t.Fatalf("the ceiling as packets of 1500 bytes: %v", f.rate)
	}
	s.set(bandwidth.Limits{})
	if f.rate != 0 {
		t.Fatalf("no limits: the configured packet rate again: %v", f.rate)
	}
}

func TestShaperKeepsTheConfiguredPacketRateWithTheByteLimiter(t *testing.T) {
	f := &fakePath{limiter: true, active: 1}
	s := &fastShaper{fp: f, base: 3000, burst: 90, log: logr.Discard()}
	s.set(bandwidth.Limits{CapKbps: 50_000})
	if f.rate != 3000 || f.burst != 90 {
		t.Fatalf("packet bucket: %v %d", f.rate, f.burst)
	}
	if f.session != 50_000 || f.class != 0 {
		t.Fatalf("only the cap, no class: %d %d", f.session, f.class)
	}
}

func TestShaperInUserspaceModeDoesNothing(t *testing.T) {
	s := &fastShaper{log: logr.Discard()}
	s.set(bandwidth.Limits{ClassKbps: 1, CapKbps: 1}) // must not panic
}
