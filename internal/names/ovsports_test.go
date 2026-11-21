package names

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
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

// The object names of a gateway cannot be the names of another lab's gateway, even when lab and gateway names contain "-".
func TestGatewayObjectNamesAreUnambiguous(t *testing.T) {
	if LabVPNObjectName("a", "b-c") == LabVPNObjectName("a-b", "c") || LabGatewayObjectName("a", "b-c") == LabGatewayObjectName("a-b", "c") ||
		DHCPPoolName("a", "b-c") == DHCPPoolName("a-b", "c") {
		t.Fatal("two gateways share an object name")
	}
	if LabVPNObjectName("lab", "attack") != "labvpn-lab.attack" || LabGatewayObjectName("lab", "out") != "labgw-lab.out" || DHCPPoolName("lab", "out") != "dhcp-lab.out-0" {
		t.Fatal("names changed")
	}
}

func TestDHCPPoolOwner(t *testing.T) {
	for _, tc := range []struct{ pool, lab, want string }{
		{"dhcp-lab.out-0", "lab", "out"},
		{"dhcp-lab.b-c-0", "lab", "b-c"},
		{"dhcp-lab-x.out-0", "lab", ""},
		{"dhcp-other.out-0", "lab", ""},
		{"dhcp-lab.out", "lab", ""},
		{"dhcp-lab.-0", "lab", ""},
	} {
		if got := DHCPPoolOwner(tc.pool, tc.lab); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.pool, tc.lab, got, tc.want)
		}
	}
}

// The longest names a lab can have: a lab name of 63 characters (names.EncodeName) and a gateway of 35 (MaxDeviceNameLen). Every name made
// from the pair must be valid for what carries it: object names are DNS subdomains (253), the pool name too, and a label value is 63.
func TestGatewayNamesAtTheLimits(t *testing.T) {
	gateway := "g" + strings.Repeat("a", MaxDeviceNameLen-2) + "z"
	for _, lab := range []string{
		strings.Repeat("l", 63),
		"0123456789abcdef-0123-0123-0123456789ab", // a UUID-length id
		EncodeName(strings.Repeat("X", 200)),      // an id that does not fit is hashed to 52 characters
	} {
		if len(lab) > 63 || len(gateway) != MaxDeviceNameLen {
			t.Fatalf("test input: lab %d gateway %d", len(lab), len(gateway))
		}
		for what, name := range map[string]string{
			"LabVPN":     LabVPNObjectName(lab, gateway),
			"LabGateway": LabGatewayObjectName(lab, gateway),
			"DHCP pool":  DHCPPoolName(lab, gateway),
		} {
			if errs := validation.IsDNS1123Subdomain(name); len(errs) != 0 {
				t.Errorf("%s name %q (%d characters) is not a valid object name: %v", what, name, len(name), errs)
			}
		}
		if got := DHCPPoolOwner(DHCPPoolName(lab, gateway), lab); got != gateway {
			t.Errorf("the pool name of lab %q does not give its gateway back: %q", lab, got)
		}
		group := DHCPPoolGroup(lab, gateway)
		if errs := validation.IsValidLabelValue(group); len(errs) != 0 || len(group) > 63 {
			t.Errorf("pool group %q (%d characters) is not a valid label value: %v", group, len(group), errs)
		}
		if group == DHCPPoolGroup(lab, gateway[:len(gateway)-1]+"y") {
			t.Errorf("two gateways of lab %q share the pool group %q", lab, group)
		}
	}
	// A pair that fits is not changed, so the value stays readable.
	if g := DHCPPoolGroup("lab1", "entry"); g != "dhcp-lab1.entry" {
		t.Errorf("a short group is %q", g)
	}
}
