package l7

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestAttemptIsVisibleBeforeCompletion(t *testing.T) {
	m := NewMeter("b", time.Unix(1, 0))
	r := m.Begin("ns", "p", "lab", time.Unix(2, 0))
	r.AddIn(5)
	r.AddOut(3)
	r.Responded(time.Unix(4, 0))
	rows, truncated, partial := m.Snapshot("ns")
	if truncated || partial || len(rows) != 1 || rows[0].Attempts != 1 || rows[0].BytesIn != 5 || rows[0].BytesOut != 3 || rows[0].RespondedMs != 4000 {
		t.Fatal(rows, truncated, partial)
	}
	r.End()
	r.End()
	r.AddIn(10)
	rows, _ = m.Ledger("ns")
	if rows[0].BytesIn != 5 || rows[0].Attempts != 1 {
		t.Fatal(rows)
	}
}
func TestProgressMetersClampOverflowAndScopeIncomplete(t *testing.T) {
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Unix(2, 0))
	r.AddIn(math.MaxInt64)
	r.AddIn(1)
	r.AddOut(-1)
	r.Incomplete()
	r.End()
	rows, truncated, partial := m.Snapshot("ns")
	if truncated || !partial || rows[0].BytesIn != math.MaxInt64 || rows[0].BytesOut != 0 {
		t.Fatal(rows, truncated, partial)
	}
	_, truncated, partial = m.Snapshot("other")
	if truncated || partial {
		t.Fatal("flags leaked across groups")
	}
}
func TestRestoreAddsOnceWithoutDroppingNewProgress(t *testing.T) {
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Unix(9, 0))
	r.AddIn(2)
	old := []Touch{{Subject: "p", Lab: "lab", Attempts: 8, BytesIn: 80, FirstSeenMs: 1000, LastSeenMs: 5000, RespondedMs: 2000}}
	m.Restore("ns", old, true, false)
	m.Restore("ns", old, true, false)
	rows, truncated, partial := m.Snapshot("ns")
	if !truncated || partial || len(rows) != 1 || rows[0].Attempts != 9 || rows[0].BytesIn != 82 || rows[0].FirstSeenMs != 1000 || rows[0].LastSeenMs != 9000 || rows[0].RespondedMs != 2000 {
		t.Fatal(rows, truncated, partial)
	}
	r.End()
}
func TestRetirementKeepsActiveCountersUntilSettled(t *testing.T) {
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Now())
	m.RetireNamespaces(map[string]bool{})
	r.AddIn(4)
	rows, _ := m.Ledger("ns")
	if len(rows) != 1 || rows[0].BytesIn != 4 {
		t.Fatal(rows)
	}
	r.End()
	rows, _ = m.Ledger("ns")
	if len(rows) != 0 {
		t.Fatal("inactive retired namespace retained", rows)
	}
}
func TestProgressConcurrentHandlesAndSnapshot(t *testing.T) {
	m := NewMeter("b", time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := m.Begin("ns", "p", "lab", time.Now())
			for j := 0; j < 100; j++ {
				r.AddIn(2)
				r.AddOut(1)
				m.Snapshot("ns")
			}
			r.End()
		}()
	}
	wg.Wait()
	rows, _ := m.Ledger("ns")
	if len(rows) != 1 || rows[0].Attempts != 20 || rows[0].BytesIn != 4000 || rows[0].BytesOut != 2000 {
		t.Fatal(rows)
	}
}
