package flowacct

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

// benchCollector is the realistic load of a group: clients, each with flows to lab addresses of one /24, every flow moving bytes.
func benchCollector(b *testing.B, clients, flowsEach int, touches bool) (*Collector, *fakeSource) {
	b.Helper()
	topology := Topology{Clients: map[netip.Addr]string{}, Labs: []LabNet{{Name: "c-1", Index: 1, Prefix: netip.MustParsePrefix("10.9.1.0/24")}}}
	var rows []PairCounters
	src := &fakeSource{}
	id := uint32(0)
	for i := 0; i < clients; i++ {
		ip := netip.AddrFrom4([4]byte{10, 8, 0, byte(2 + i)})
		name := fmt.Sprintf("p-%d", i)
		topology.Clients[ip] = name
		rows = append(rows, PairCounters{Key: Key{name, "c-1"}, BindingID: "b" + name, ClientCIDR: ip.String() + "/32", Epoch: "e", PacketsOut: 1, Attempts: 1})
		for j := 0; j < flowsEach; j++ {
			id++
			f := Flow{ID: id, Proto: "tcp", Src: ip, Dst: netip.AddrFrom4([4]byte{10, 9, 1, byte(1 + j%250)}), SrcPort: 40000 + uint16(j), DstPort: 80, Mark: 0x80000100, BytesOut: 1000, BytesIn: 5000}
			src.flows = append(src.flows, f)
		}
	}
	c := New(src, func() Topology { return topology }, "boot", 5*time.Second)
	if touches {
		c.EnableTouches()
	}
	if err := c.ObserveCounters(CounterSnapshot{Rows: rows, At: t0}); err != nil {
		b.Fatal(err)
	}
	return c, src
}

// The poll of the conntrack flows (every 5 s) with and without the per-address touches: 15 clients with 50 flows each.
func BenchmarkPollTouches(b *testing.B) {
	for _, touches := range []bool{false, true} {
		b.Run(fmt.Sprintf("touches=%v/15clients_x_50flows", touches), func(b *testing.B) {
			c, src := benchCollector(b, 15, 50, touches)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for k := range src.flows {
					src.flows[k].BytesOut += 100
					src.flows[k].BytesIn += 500
				}
				if err := c.Poll(t0.Add(time.Duration(i) * 5 * time.Second)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
