package flowacct

import (
	"net/netip"
	"testing"
	"time"
)

func touchFlow(id uint32, src, dst netip.Addr, out, in uint64) Flow {
	f := flow(id, src, dst, 80)
	f.Mark, f.BytesOut, f.BytesIn = 0x80000100, out, in
	return f
}

// Without EnableTouches nothing is recorded or kept.
func TestTouchesAreOffByDefault(t *testing.T) {
	src := &fakeSource{flows: []Flow{touchFlow(1, clientA, labIP, 100, 50)}}
	c := newCollector(src)
	observe(t, c, sample(2), t0)
	if err := c.Poll(t0); err != nil {
		t.Fatal(err)
	}
	if got := c.TakeTouches(t0.Add(2 * time.Minute)); len(got) != 0 {
		t.Fatalf("touches = %+v", got)
	}
}

// The bytes a flow moves are added per minute and per (client, lab address); a finished minute is taken once.
func TestTouchesCountDeltasPerMinuteAndPerAddress(t *testing.T) {
	other := netip.MustParseAddr("10.9.1.6")
	src := &fakeSource{flows: []Flow{touchFlow(1, clientA, labIP, 100, 40), touchFlow(2, clientA, other, 7, 0), touchFlow(3, clientB, labIP, 5, 5)}}
	c := newCollector(src)
	c.EnableTouches()
	observe(t, c, sample(2), t0)
	if err := c.Poll(t0); err != nil {
		t.Fatal(err)
	}
	src.flows[0].BytesOut, src.flows[0].BytesIn = 160, 90 // +60 out, +50 in
	if err := c.Poll(t0.Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := c.TakeTouches(t0.Add(30 * time.Second)); len(got) != 0 {
		t.Fatalf("the minute is not over: %+v", got)
	}
	got := c.TakeTouches(t0.Add(time.Minute))
	// p-b has no permitted binding in the sample, so its flow is not counted
	if len(got) != 2 {
		t.Fatalf("windows = %+v", got)
	}
	var a TouchWindow
	for _, w := range got {
		if w.Subject == "p-a" && w.Dst == labIP.String() {
			a = w
		}
	}
	if a.BytesOut != 160 || a.BytesIn != 90 || a.Lab != "c-1" || a.MinuteMs != t0.UnixMilli() || a.Proto != "tcp" {
		t.Fatalf("window = %+v", a)
	}
	if again := c.TakeTouches(t0.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("taken twice: %+v", again)
	}
}

// Traffic that is not a permitted client-to-lab flow is never recorded, and a flow that goes quiet adds nothing.
func TestTouchesIgnoreForeignFlowsAndQuietOnes(t *testing.T) {
	foreign := flow(9, netip.MustParseAddr("203.0.113.1"), clientA, 51820)
	foreign.Mark, foreign.BytesOut = 0x80000100, 999
	unmarked := flow(8, clientA, labIP, 80)
	unmarked.BytesOut = 999
	src := &fakeSource{flows: []Flow{foreign, unmarked, touchFlow(1, clientA, labIP, 10, 0)}}
	c := newCollector(src)
	c.EnableTouches()
	observe(t, c, sample(2), t0)
	for i := 0; i < 3; i++ {
		if err := c.Poll(t0.Add(time.Duration(i) * 5 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got := c.TakeTouches(t0.Add(time.Minute))
	if len(got) != 1 || got[0].BytesOut != 10 {
		t.Fatalf("windows = %+v", got)
	}
}

// The addresses a client can touch are bounded by the lab network, and the table by MaxTouchWindows.
func TestTouchWindowsAreBounded(t *testing.T) {
	c := newCollector(&fakeSource{})
	c.EnableTouches()
	for i := 0; i < MaxTouchWindows+50; i++ {
		c.noteTouch(t0, "p-a", "c-1", Flow{Proto: "tcp", Dst: netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), BytesOut: 1}, flowKey{id: uint32(i)})
	}
	if len(c.touchBucket) > MaxTouchWindows {
		t.Fatalf("bucket = %d", len(c.touchBucket))
	}
}
