package vpn

import (
	"math"

	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

// addTrafficCounter adds a counter to the one of the same identifier. Several relations share an identifier: a client reaches a lab through
// each of its VPN gateways, and the counters of the lab are the sum of theirs.
func addTrafficCounter(counters map[string]TrafficCounter, id string, add TrafficCounter) {
	c := counters[id]
	c.Packets, c.Bytes = saturatingAdd(c.Packets, add.Packets), saturatingAdd(c.Bytes, add.Bytes)
	counters[id] = c
}

// sumPairCounters turns the counters of the kernel relations (one per client and VPN gateway of a lab) into the counters of the access of a
// client to a lab, summed over the gateways.
func sumPairCounters(rows []flowacct.PairCounters) map[string]TrafficCounter {
	result := map[string]TrafficCounter{}
	for _, row := range rows {
		id := AccessRule{ClientName: row.Subject, LabName: row.Lab, Action: AccessAllow}.Identifier()
		addTrafficCounter(result, id, TrafficCounter{Packets: counterInt64(row.PacketsOut), Bytes: counterInt64(row.BytesOut)})
	}
	return result
}

func counterInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// saturatingAdd adds two non-negative counters and stops at the largest value instead of wrapping.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
