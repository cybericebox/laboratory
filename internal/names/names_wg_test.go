package names

import "testing"

func TestHostNetworkDemux(t *testing.T) {
	for mode, want := range map[string]bool{"tc": true, "xdp": true, "userspace": false, "": false, "foo": false} {
		if got := HostNetworkDemux(mode); got != want {
			t.Errorf("%q: %v", mode, got)
		}
	}
}
