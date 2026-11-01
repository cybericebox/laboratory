package gateway

import "testing"

func TestShapeParams(t *testing.T) {
	rate, burst, limit, policeBurst := shapeParams(50_000) // 50 Mbit/s
	if rate != 6_250_000 {
		t.Fatalf("rate %d bytes/s", rate)
	}
	if burst != 625_000 {
		t.Fatalf("burst is a tenth of a second: %d", burst)
	}
	if limit != 625_000+312_500 {
		t.Fatalf("the queue adds 50 ms: %d", limit)
	}
	if policeBurst != 2_500_000 {
		t.Fatalf("the police burst is 400 ms: %d", policeBurst)
	}
	// a very low rate still has room for a 64 kB segment
	if _, burst, _, pb := shapeParams(100); burst < 65535 || pb < 256<<10 {
		t.Fatalf("burst %d cannot hold one segment", burst)
	}
}
