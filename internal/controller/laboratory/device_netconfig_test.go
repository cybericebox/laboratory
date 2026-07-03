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

func devWith(preset laboratoryv1alpha1.SecurityPreset, ifaces []laboratoryv1alpha1.InterfaceSpec) *laboratoryv1alpha1.Device {
	d := &laboratoryv1alpha1.Device{}
	d.Spec.SecurityPreset = preset
	d.Spec.Interfaces = ifaces
	return d
}

func TestDeviceSecurityContext(t *testing.T) {
	// basic/empty preset, no dhcp → nil (fully unprivileged).
	if deviceSecurityContext(devWith("", nil)) != nil {
		t.Error("basic device must have nil SecurityContext")
	}
	if deviceSecurityContext(devWith(laboratoryv1alpha1.SecurityPresetBasic, nil)) != nil {
		t.Error("explicit basic preset must have nil SecurityContext")
	}
	// service preset → NET_BIND_SERVICE only.
	sc := deviceSecurityContext(devWith(laboratoryv1alpha1.SecurityPresetService, nil))
	if sc == nil || len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_BIND_SERVICE" {
		t.Errorf("service preset = %+v, want [NET_BIND_SERVICE]", sc)
	}
	// net preset → includes NET_ADMIN + NET_RAW.
	sc = deviceSecurityContext(devWith(laboratoryv1alpha1.SecurityPresetNet, nil))
	got := map[corev1.Capability]bool{}
	for _, c := range sc.Capabilities.Add {
		got[c] = true
	}
	if !got["NET_ADMIN"] || !got["NET_RAW"] {
		t.Errorf("net preset missing NET_ADMIN/NET_RAW: %+v", sc.Capabilities.Add)
	}
	// In-image dhcp interface implies NET_ADMIN+NET_RAW even on basic preset.
	dhcp := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCP}}}
	sc = deviceSecurityContext(devWith("", dhcp))
	got = map[corev1.Capability]bool{}
	for _, c := range sc.Capabilities.Add {
		got[c] = true
	}
	if !got["NET_ADMIN"] || !got["NET_RAW"] {
		t.Errorf("in-image dhcp caps = %+v, want NET_ADMIN+NET_RAW", sc.Capabilities.Add)
	}
	// dhcp-preset (platform-managed) interface → NO device caps (init does it).
	preset := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCPPreset}}}
	if deviceSecurityContext(devWith("", preset)) != nil {
		t.Error("dhcp-preset device must have nil SecurityContext")
	}
	// static interface → no implied caps.
	static := []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.1/24"}}}
	if deviceSecurityContext(devWith("", static)) != nil {
		t.Error("static device must have nil SecurityContext")
	}
}
