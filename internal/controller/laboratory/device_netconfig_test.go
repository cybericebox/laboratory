package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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

// Every capability is dropped; the base set, the preset's and the DHCP ones come back, and nothing else.
func TestDeviceSecurityContext(t *testing.T) {
	base := DefaultDeviceBaseCaps
	basic := deviceSecurityContext(devWith("", nil), base)
	if len(basic.Capabilities.Drop) != 1 || basic.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("drop = %v, want [ALL]", basic.Capabilities.Drop)
	}
	got := capSet(basic)
	if len(got) != len(base) {
		t.Errorf("basic device keeps %v, want exactly the base set %v", got, base)
	}
	for _, denied := range []corev1.Capability{"NET_RAW", "MKNOD", "SETFCAP", "NET_ADMIN", "SYS_ADMIN", "SYS_PTRACE"} {
		if got[denied] {
			t.Errorf("basic device must not keep %s", denied)
		}
	}
	// A device may escalate (sudo, setuid binaries): no_new_privs is not set.
	if basic.AllowPrivilegeEscalation != nil || basic.Privileged != nil {
		t.Errorf("unexpected escalation settings: %+v", basic)
	}
	// service preset adds nothing new (NET_BIND_SERVICE is in the base); the base is configurable.
	if got := capSet(deviceSecurityContext(devWith(laboratoryv1alpha1.SecurityPresetService, nil), []string{"CHOWN"})); len(got) != 2 || !got["CHOWN"] || !got["NET_BIND_SERVICE"] {
		t.Errorf("service preset over a one-capability base = %v", got)
	}
	// net preset adds NET_ADMIN + NET_RAW.
	if got := capSet(deviceSecurityContext(devWith(laboratoryv1alpha1.SecurityPresetNet, nil), base)); !got["NET_ADMIN"] || !got["NET_RAW"] || !got["CHOWN"] {
		t.Errorf("net preset = %v", got)
	}
	// In-image dhcp interface implies NET_ADMIN+NET_RAW even on the basic preset.
	dhcp := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCP}}}
	if got := capSet(deviceSecurityContext(devWith("", dhcp), base)); !got["NET_ADMIN"] || !got["NET_RAW"] {
		t.Errorf("in-image dhcp caps = %v, want NET_ADMIN+NET_RAW", got)
	}
	// dhcp-preset (platform-managed) and static interfaces add no device caps (the init container does it).
	preset := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCPPreset}}}
	static := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.1/24"}}}
	for name, ifaces := range map[string][]laboratoryv1alpha1.InterfaceSpec{"dhcp-preset": preset, "static": static} {
		if got := capSet(deviceSecurityContext(devWith("", ifaces), base)); got["NET_ADMIN"] || got["NET_RAW"] || len(got) != len(base) {
			t.Errorf("%s device keeps %v", name, got)
		}
	}
}
