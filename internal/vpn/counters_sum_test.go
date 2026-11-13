package vpn

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

// A client reaches a lab through every VPN gateway of it: the counters of the lab are the sum of the counters of the relations, never the
// last one read.
func TestPairCountersOfTheGatewaysOfALabAreSummed(t *testing.T) {
	row := func(client, lab, binding string, packets, bytes uint64) flowacct.PairCounters {
		return flowacct.PairCounters{Key: flowacct.Key{Subject: client, Lab: lab}, BindingID: binding, PacketsOut: packets, BytesOut: bytes}
	}
	got := sumPairCounters([]flowacct.PairCounters{
		row("c", "a", "b1", 10, 100), row("c", "a", "b2", 5, 50), row("c", "b", "b3", 7, 70), row("d", "a", "b4", 1, 1),
	})
	id := func(client, lab string) string {
		return AccessRule{ClientName: client, LabName: lab, Action: AccessAllow}.Identifier()
	}
	if c := got[id("c", "a")]; c.Packets != 15 || c.Bytes != 150 {
		t.Fatalf("two gateways of one lab: %+v", c)
	}
	if got[id("c", "b")].Packets != 7 || got[id("d", "a")].Packets != 1 || len(got) != 3 {
		t.Fatalf("other pairs are separate: %+v", got)
	}
	if c := sumPairCounters([]flowacct.PairCounters{row("c", "a", "b1", 1<<63+5, 0), row("c", "a", "b2", 1<<63, 0)})[id("c", "a")]; c.Packets != 1<<63-1 {
		t.Fatalf("a huge counter must not wrap: %+v", c)
	}
	counters := map[string]TrafficCounter{}
	addTrafficCounter(counters, "x", TrafficCounter{Packets: 2, Bytes: 3})
	addTrafficCounter(counters, "x", TrafficCounter{Packets: 4, Bytes: 5})
	if counters["x"] != (TrafficCounter{Packets: 6, Bytes: 8}) {
		t.Fatalf("%+v", counters)
	}
}
