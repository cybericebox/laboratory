package demux

import (
	"net"
	"runtime"
	"testing"
)

// D-8: what the conntrack table costs at its cap: the bytes per entry, measured with MaxEntries entries in the table (two per session).
// The numbers behind proxy.wg.resources in the chart (DEPLOY.md, "Sizing the proxy").
func TestConntrackMemoryAtTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("load measurement")
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	l := DefaultLimits()
	c := NewConnTrackWithLimits(l)
	before := heap()
	backend := Socket{IP: net.IPv4(10, 0, 0, 1), Port: 51820}
	sessions := l.MaxEntries / 2
	for i := 0; i < sessions; i++ {
		client := Socket{IP: net.IPv4(byte(i>>16), byte(i>>8), byte(i), 7), Port: uint16(1024 + i%60000)}
		ci, si := uint32(2*i+1), uint32(2*i+2)
		if !c.AddPartial(ci, client, backend) || !c.Complete(si, ci, client, backend) {
			t.Fatalf("session %d refused before the cap", i)
		}
	}
	after := heap()
	if c.Len() != l.MaxEntries {
		t.Fatalf("entries = %d, want %d", c.Len(), l.MaxEntries)
	}
	perEntry := float64(after-before) / float64(c.Len())
	t.Logf("%d entries: %.1f MiB, %.0f bytes per entry", c.Len(), float64(after-before)/(1<<20), perEntry)
	if perEntry > 600 {
		t.Errorf("an entry costs %.0f bytes, the chart's sizing assumes at most 600", perEntry)
	}
	runtime.KeepAlive(c)
}
