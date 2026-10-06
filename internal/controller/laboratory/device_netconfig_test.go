package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/profiles"
)

func TestValidIfaceName(t *testing.T) {
	good := []string{"eth0", "eth1", "lab7", "gw254", "a", "e_th-0.1"}
	for _, n := range good {
		if _, ok := validIfaceName(n); !ok {
			t.Errorf("expected %q valid", n)
		}
	}
	bad := []string{"", "eth0; rm -rf /", "$(reboot)", "a b", "eth0\nrm", "toolonginterfacename", "-eth0", "a|b"}
	for _, n := range bad {
		if _, ok := validIfaceName(n); ok {
			t.Errorf("expected %q rejected", n)
		}
	}
}

func TestCanonicalCIDR(t *testing.T) {
	if got, ok := canonicalCIDR("10.10.1.10/24"); !ok || got != "10.10.1.10/24" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	// Injection attempts and malformed values must be rejected.
	for _, s := range []string{"10.0.0.1/24; reboot", "$(id)", "10.0.0.1", "notanip/24", ""} {
		if _, ok := canonicalCIDR(s); ok {
			t.Errorf("expected %q rejected", s)
		}
	}
}

func TestCanonicalIP(t *testing.T) {
	if got, ok := canonicalIP("10.10.1.1"); !ok || got != "10.10.1.1" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	for _, s := range []string{"10.0.0.1 && curl evil", "$(reboot)", "10.0.0.1/24", ""} {
		if _, ok := canonicalIP(s); ok {
			t.Errorf("expected %q rejected", s)
		}
	}
}

func devWith(
	preset laboratoryv1alpha1.SecurityPreset,
	ifaces []laboratoryv1alpha1.InterfaceSpec,
) *laboratoryv1alpha1.Device {
	d := &laboratoryv1alpha1.Device{}
	d.Spec.SecurityPreset = preset
	d.Spec.Interfaces = ifaces
	return d
}

func capSet(sc *corev1.SecurityContext) map[corev1.Capability]bool {
	got := map[corev1.Capability]bool{}
	for _, c := range sc.Capabilities.Add {
		got[c] = true
	}
	return got
}

// Every capability is dropped; the base set, the profile's and the DHCP ones come back, and nothing else.
func TestDeviceSecurityContext(t *testing.T) {
	base := profiles.Base
	std := deviceSecurityContext(devWith("", nil))
	if len(std.Capabilities.Drop) != 1 || std.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("drop = %v, want [ALL]", std.Capabilities.Drop)
	}
	got := capSet(std)
	// the standard profile: the base set plus SYS_PTRACE, IPC_LOCK, LINUX_IMMUTABLE
	if len(got) != len(base)+3 || !got["SYS_PTRACE"] || !got["IPC_LOCK"] || !got["LINUX_IMMUTABLE"] {
		t.Errorf("standard device keeps %v", got)
	}
	for _, denied := range append([]string{"NET_RAW", "NET_ADMIN"}, profiles.Never...) {
		if got[corev1.Capability(denied)] {
			t.Errorf("a standard device must not keep %s", denied)
		}
	}
	// A device may escalate (sudo, setuid binaries): no_new_privs is not set.
	if std.AllowPrivilegeEscalation != nil || std.Privileged != nil {
		t.Errorf("unexpected escalation settings: %+v", std)
	}
	// The old names are aliases: basic and service are standard, net and debug are extended.
	for _, old := range []laboratoryv1alpha1.SecurityPreset{laboratoryv1alpha1.SecurityPresetBasic, laboratoryv1alpha1.SecurityPresetService, laboratoryv1alpha1.SecurityPresetStandard} {
		if g := capSet(deviceSecurityContext(devWith(old, nil))); len(g) != len(got) || g["NET_RAW"] {
			t.Errorf("%s must mean standard: %v", old, g)
		}
	}
	for _, ext := range []laboratoryv1alpha1.SecurityPreset{laboratoryv1alpha1.SecurityPresetNet, laboratoryv1alpha1.SecurityPresetDebug, laboratoryv1alpha1.SecurityPresetExtended} {
		g := capSet(deviceSecurityContext(devWith(ext, nil)))
		if !g["NET_ADMIN"] || !g["NET_RAW"] || !g["SYS_PTRACE"] || !g["CHOWN"] || len(g) != len(base)+5 {
			t.Errorf("%s must mean extended: %v", ext, g)
		}
		for _, denied := range profiles.Never {
			if g[corev1.Capability(denied)] {
				t.Errorf("extended must not keep %s", denied)
			}
		}
	}
	// In-image dhcp interface implies NET_ADMIN+NET_RAW even on the standard profile.
	dhcp := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCP}}}
	if g := capSet(deviceSecurityContext(devWith("", dhcp))); !g["NET_ADMIN"] || !g["NET_RAW"] {
		t.Errorf("in-image dhcp caps = %v, want NET_ADMIN+NET_RAW", g)
	}
	// dhcp-preset (platform-managed) and static interfaces add no device caps (the init container does it).
	preset := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCPPreset}}}
	static := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.1/24"}}}
	for name, ifaces := range map[string][]laboratoryv1alpha1.InterfaceSpec{"dhcp-preset": preset, "static": static} {
		if g := capSet(deviceSecurityContext(devWith("", ifaces))); g["NET_ADMIN"] || g["NET_RAW"] {
			t.Errorf("%s device keeps %v", name, g)
		}
	}
}
