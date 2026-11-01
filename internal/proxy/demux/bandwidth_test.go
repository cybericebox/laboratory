package demux

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

func TestPacketRateFor(t *testing.T) {
	for name, tc := range map[string]struct {
		base float64
		l    bandwidth.Limits
		want float64
	}{
		"no limits keeps the configured rate": {500, bandwidth.Limits{}, 500},
		"no limits, nothing configured":       {0, bandwidth.Limits{}, 0},
		"50 Mbit/s of 1500-byte packets":      {0, bandwidth.Limits{CapKbps: 50_000}, 4166.666666666667},
		"the configured rate is smaller":      {1000, bandwidth.Limits{CapKbps: 50_000}, 1000},
		"the ceiling is smaller":              {10000, bandwidth.Limits{CapKbps: 50_000}, 4166.666666666667},
		"only a class: the class is the cap":  {0, bandwidth.Limits{ClassKbps: 12_000}, 1000},
	} {
		if got := PacketRateFor(tc.base, tc.l); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestPacketBurstFor(t *testing.T) {
	if PacketBurstFor(0, 7) != 7 || PacketBurstFor(4000, 7) != 400 || PacketBurstFor(50, 0) != 16 || PacketBurstFor(4000, 1000) != 1000 {
		t.Fatalf("%d %d %d %d", PacketBurstFor(0, 7), PacketBurstFor(4000, 7), PacketBurstFor(50, 0), PacketBurstFor(4000, 1000))
	}
}
