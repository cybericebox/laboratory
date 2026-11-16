package egressproxy

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

type addrConn struct {
	net.Conn
	addr net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.addr }

func benchProxy(detail bool) (*Proxy, net.Conn, []net.Conn) {
	stats := NewCollector("g")
	stats.Detail = detail
	p := &Proxy{Stats: stats, Log: logr.Discard()}
	from := addrConn{addr: &net.TCPAddr{IP: net.IPv4(10, 9, 1, 5), Port: 40000}}
	var ups []net.Conn
	for i := 0; i < 200; i++ { // a lab talks to a couple of hundred addresses
		ups = append(ups, addrConn{addr: &net.TCPAddr{IP: net.IPv4(151, 101, byte(i>>8), byte(i)), Port: 443}})
	}
	return p, from, ups
}

// The work added to the close of every proxied connection (the hot path): with activity reports off and on.
func BenchmarkRecordFlowOnSpliceClose(b *testing.B) {
	for _, detail := range []bool{false, true} {
		b.Run(fmt.Sprintf("detail=%v", detail), func(b *testing.B) {
			p, from, ups := benchProxy(detail)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.recordFlow("lab-a", from, ups[i%len(ups)], 1000, 5000)
			}
		})
	}
}

// Off, it does nothing: no row, no map, no allocation.
func TestRecordFlowIsAFullNoOpWhenOff(t *testing.T) {
	p, from, ups := benchProxy(false)
	if allocs := testing.AllocsPerRun(100, func() { p.recordFlow("lab-a", from, ups[0], 1, 1) }); allocs != 0 {
		t.Fatalf("%v allocations with reports off", allocs)
	}
	p.Stats.Now = func() time.Time { return time.Unix(0, 0) }
	p.recordFlow("lab-a", from, ups[0], 1, 1)
	if p.Stats.flows != nil || p.Stats.rows != nil {
		t.Fatal("a map was made with reports off")
	}
	// even a finished minute makes no flow map
	p.Stats.Now = func() time.Time { return time.Unix(120, 0) }
	p.Stats.Start("lab-a", "x")
	if p.Stats.flows != nil {
		t.Fatal("the minute roll made a flow map with reports off")
	}
}
