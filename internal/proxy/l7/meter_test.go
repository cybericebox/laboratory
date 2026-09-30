package l7

import (
	"strconv"
	"testing"
	"time"
)

func TestMeterKeepsFirstLastAndSums(t *testing.T) {
	m := NewMeter("b", time.Now())
	t1, t2, t3 := time.UnixMilli(5_000), time.UnixMilli(9_000), time.UnixMilli(1_000)
	m.Record("ns", "u1", "c-1", t1, false, 10, 1)
	m.Record("ns", "u1", "c-1", t2, true, 20, 2)
	m.Record("ns", "u1", "c-1", t3, true, 5, 0)
	m.Record("ns", "u2", "c-1", t1, true, 1, 1)
	m.Record("other", "u1", "c-1", t1, true, 1, 1)

	rows, truncated := m.Ledger("ns")
	if truncated || len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	u1 := rows[0]
	if u1.Subject != "u1" || u1.Attempts != 3 || u1.BytesIn != 35 || u1.BytesOut != 3 || u1.FirstSeenMs != 1_000 || u1.LastSeenMs != 9_000 {
		t.Fatalf("u1 = %+v", u1)
	}
	if u1.RespondedMs != 1_000 {
		t.Fatalf("first response = %d, want the earliest answered request", u1.RespondedMs)
	}
}

func TestMeterBoundsItsMemoryAndSaysSo(t *testing.T) {
	m := NewMeter("b", time.Now())
	for i := 0; i < MaxMeterKeys+5; i++ {
		m.Record("ns", strconv.Itoa(i), "c-1", time.Now(), true, 0, 0)
	}
	m.Record("ns2", "u", "c-1", time.Now(), true, 0, 0)
	if rows, truncated := m.Ledger("ns2"); len(rows) != 0 || !truncated {
		t.Fatalf("beyond the cap nothing may be added silently: %+v %v", rows, truncated)
	}
}
