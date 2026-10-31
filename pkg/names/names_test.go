package names

import "testing"

func TestReservedDeviceNames(t *testing.T) {
	want := map[string]bool{"vpn": true, "gateway": true, "internet": true}
	if len(ReservedDeviceNames) != len(want) {
		t.Fatalf("got %v", ReservedDeviceNames)
	}
	for _, n := range ReservedDeviceNames {
		if !want[n] {
			t.Errorf("unexpected %q", n)
		}
	}
}
