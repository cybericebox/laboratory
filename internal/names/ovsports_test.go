package names

import (
	"fmt"
	"testing"
)

func TestDevicePortKeyDeterministic(t *testing.T) {
	a := DevicePortKey("ns1", "pod-a", "eth1")
	b := DevicePortKey("ns1", "pod-a", "eth1")
	if a != b {
		t.Fatalf("not deterministic: %s vs %s", a, b)
	}
}

func TestDevicePortKeyLength(t *testing.T) {
	key := DevicePortKey("very-long-namespace-name", "very-long-pod-name-with-suffix", "eth10")
	if len(key) > 15 {
		t.Fatalf("key %q exceeds IFNAMSIZ-1 (15): %d", key, len(key))
	}
	// The veth peer adds a "v" prefix and must also fit.
	if len("v"+key) > 15 {
		t.Fatalf("veth peer name v%s exceeds IFNAMSIZ-1 (15)", key)
	}
}

func TestDevicePortKeyUniqueAcrossInputs(t *testing.T) {
	seen := map[string]string{}
	for ns := 0; ns < 10; ns++ {
		for pod := 0; pod < 20; pod++ {
			for _, iface := range []string{"eth0", "eth1", "eth2"} {
				id := fmt.Sprintf("ns%d/pod%d/%s", ns, pod, iface)
				key := DevicePortKey(fmt.Sprintf("ns%d", ns), fmt.Sprintf("pod%d", pod), iface)
				if prev, dup := seen[key]; dup {
					t.Fatalf("collision: %s and %s both map to %s", prev, id, key)
				}
				seen[key] = id
			}
		}
	}
}

func TestLabAndGWIfaceNamesDistinct(t *testing.T) {
	// VPN and gateway legs of the same lab share NetworkIndex N; their host-side
	// OVS port names must never collide in the root netns.
	for n := uint(1); n <= 254; n++ {
		if LabIfaceNameByIndex(n) == GWIfaceNameByIndex(n) {
			t.Fatalf("lab and gw host-side names collide for index %d", n)
		}
	}
	if got := LabIfaceNameByIndex(7); got != "lab7" {
		t.Fatalf("LabIfaceNameByIndex(7) = %q", got)
	}
	if got := GWIfaceNameByIndex(7); got != "gw7" {
		t.Fatalf("GWIfaceNameByIndex(7) = %q", got)
	}
}
