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

func TestParseUnavailableReason(t *testing.T) {
	code, n, m, err := ParseUnavailableReason("SNAPSHOT_REGISTRY_UNREACHABLE:3/4")
	if err != nil || code != ReasonSnapshotRegistry || n != 3 || m != 4 {
		t.Fatalf("%q %d %d %v", code, n, m, err)
	}
	for _, bad := range []string{"", "node-1: the root /x is missing", "CONTAINERD_ROOT_MISSING", "CONTAINERD_ROOT_MISSING:1", "CONTAINERD_ROOT_MISSING:5/4", "CONTAINERD_ROOT_MISSING:1/0", "OTHER:1/2", "container_root_missing:1/2"} {
		if _, _, _, err := ParseUnavailableReason(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(UnavailableReasonCodes) != 8 {
		t.Errorf("the allow-list has 8 codes: %v", UnavailableReasonCodes)
	}
}
