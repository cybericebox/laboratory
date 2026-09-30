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

func TestHostPortKeysAreUniquePerGroupAndLeg(t *testing.T) {
	// Lab indexes are allocated per group, so every group has a lab 1. Host-side
	// OVS ports live in one root netns per node: VPN and gateway legs, and the
	// same leg of different groups, must never share a name.
	seen := map[string]string{}
	for _, ns := range []string{"e-aaa-t-bbb", "e-aaa-t-ccc"} {
		for n := uint(1); n <= 254; n++ {
			for leg, key := range map[string]string{"vpn": VPNHostPortKey(ns, n), "gw": GWHostPortKey(ns, n)} {
				if len(key) > 15 || len(VethPeerNameForTest(key)) > 15 {
					t.Fatalf("%s key %q too long for IFNAMSIZ", leg, key)
				}
				id := fmt.Sprintf("%s/%s/%d", ns, leg, n)
				if prev, dup := seen[key]; dup {
					t.Fatalf("host port %q shared by %s and %s", key, prev, id)
				}
				seen[key] = id
			}
		}
	}
	if VPNHostPortKey("x", 1) != VPNHostPortKey("x", 1) {
		t.Fatal("VPNHostPortKey is not stable")
	}
	if got := LabIfaceNameByIndex(7); got != "lab7" {
		t.Fatalf("LabIfaceNameByIndex(7) = %q", got)
	}
}

// VethPeerNameForTest mirrors nodeagent.VethPeerName ("v" + key).
func VethPeerNameForTest(key string) string { return "v" + key }
