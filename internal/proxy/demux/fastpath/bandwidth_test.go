package fastpath

import "testing"

func TestByteLimits(t *testing.T) {
	if got := byteLimits(0, 0); got != (limitValues{}) {
		t.Fatalf("no limits: %+v", got)
	}
	// a participant at 50 Mbit/s: 6.25 MB/s, a tenth of a second of burst
	if got := byteLimits(50_000, 0); got.byteRate != 6_250_000 || got.byteBurst != 625_000 || got.windowBytes != 0 {
		t.Fatalf("session only: %+v", got)
	}
	// a class of 340 Mbit/s: 42.5 MB/s, a 1 ms window holds 42.5 kB
	if got := byteLimits(0, 340_000); got.windowNs != 1_000_000 || got.windowBytes != 42_500 || got.byteRate != 0 {
		t.Fatalf("class only: %+v", got)
	}
	// a small session rate still passes a few full packets at once
	if got := byteLimits(100, 0); got.byteBurst != 16384 {
		t.Fatalf("burst floor: %+v", got)
	}
	// a class so small that a millisecond holds less than a packet: a longer window, never fewer than two packets
	got := byteLimits(0, 1_000) // 125 kB/s
	if got.windowBytes < 3000 || got.windowNs < 1_000_000 {
		t.Fatalf("small class: %+v", got)
	}
	if rate := float64(got.windowBytes) / (float64(got.windowNs) / 1e9); rate < 100_000 || rate > 150_000 {
		t.Fatalf("the window keeps the class rate: %v B/s", rate)
	}
}
