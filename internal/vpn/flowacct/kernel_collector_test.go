package flowacct

import (
	"math"
	"testing"
	"time"
)

func sample(n uint64) PairCounters {
	return PairCounters{Key: Key{"p-a", "c-1"}, BindingID: "binding", Epoch: "epoch", PacketsOut: n, BytesOut: n * 40, Attempts: 1}
}
func observe(t *testing.T, c *Collector, row PairCounters, at time.Time) {
	t.Helper()
	if err := c.ObserveCounters(CounterSnapshot{Rows: []PairCounters{row}, At: at}); err != nil {
		t.Fatal(err)
	}
}
func TestKernelCountersSurviveShortFlowAndResume(t *testing.T) {
	c := newCollector(&fakeSource{})
	observe(t, c, sample(3), t0)
	r := c.Snapshot(t0)
	if len(r.Ledger) != 1 || r.Ledger[0].PacketsOut != 3 {
		t.Fatalf("short flow lost: %+v", r)
	}
	next := newCollector(&fakeSource{})
	next.Resume(r.Ledger, t0, r.KernelCheckpoints...)
	observe(t, next, sample(3), t0.Add(time.Second))
	if got := next.Snapshot(t0).Ledger[0].PacketsOut; got != 3 {
		t.Fatalf("restart double count %d", got)
	}
	changed := sample(2)
	changed.Epoch = "new"
	observe(t, next, changed, t0.Add(2*time.Second))
	r = next.Snapshot(t0)
	if r.Ledger[0].PacketsOut != 5 || !r.Partial {
		t.Fatalf("epoch reset %+v", r)
	}
}
func TestLabOnlyCounterNeverCreatesStudentAction(t *testing.T) {
	src := &fakeSource{flows: []Flow{flow(1, labIP, clientA, 22)}}
	c := newCollector(src)
	row := sample(0)
	row.Attempts = 0
	row.PacketsIn = 4
	row.LabInitiatedAttempts = 1
	observe(t, c, row, t0)
	_ = c.Poll(t0)
	got := c.Snapshot(t0).Ledger[0]
	if got.Attempts != 0 || got.LabInitiatedAttempts != 1 || got.PacketsIn != 4 || got.FirstSeenMs != 0 || got.FirstRespondMs != 0 {
		t.Fatalf("lab became student action %+v", got)
	}
}
func TestKernelOverflowAndTruncationAreVisible(t *testing.T) {
	c := newCollector(&fakeSource{})
	row := sample(0)
	row.PacketsOut = math.MaxUint64
	observe(t, c, row, t0)
	if r := c.Snapshot(t0); !r.Partial || r.Ledger[0].PacketsOut != math.MaxInt64 {
		t.Fatalf("overflow %+v", r)
	}
	c = newCollector(&fakeSource{})
	for i := 0; i < MaxRows+1; i++ {
		row := sample(1)
		row.Subject = string(rune(i + 1))
		row.BindingID = row.Subject
		observe(t, c, row, t0)
	}
	if r := c.Snapshot(t0); !r.Truncated || len(r.Ledger) != MaxRows || len(r.KernelCheckpoints) > MaxRows {
		t.Fatalf("unbounded ledger/checkpoints %+v", r)
	}
}
func TestUnmarkedMetadataAndReissuedIdentityCannotStealTraffic(t *testing.T) {
	src := &fakeSource{flows: []Flow{flow(1, clientA, labIP, 80)}}
	c := newCollector(src)
	observe(t, c, sample(1), t0)
	_ = c.Poll(t0)
	if got := c.Snapshot(t0).Ledger[0]; got.FirstRespondMs != 0 {
		t.Fatalf("unmarked metadata accepted %+v", got)
	}
	newOwner := sample(5)
	newOwner.Subject = "p-new"
	if err := c.ObserveCounters(CounterSnapshot{At: t0, Rows: []PairCounters{newOwner}}); err == nil {
		t.Fatal("same binding stolen by new owner")
	}
	if got := c.Snapshot(t0).Ledger[0].PacketsOut; got != 1 {
		t.Fatalf("corrupted old ledger %d", got)
	}
}
