//go:build linux

package nodeagent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"

	"github.com/cybericebox/laboratory/internal/names"
)

// R-11: only the keys the platform makes may name a veth; "eth0" never.
func TestValidPortKey(t *testing.T) {
	for _, k := range []string{
		names.DevicePortKey("ns", "pod", "eth1"), names.VPNHostPortKey("ns", 3), names.GWHostPortKey("ns", 3),
	} {
		if !ValidPortKey(k) {
			t.Errorf("%q is a key the platform makes", k)
		}
	}
	for _, k := range []string{"", "eth0", "lo", "br-ovs", "ens3", "docker0", "p1234", "pABCDEF123456", "p1234567890123", "x123456789abc", "p123456789abg", "../p123456789abc", "p123456789abc "} {
		if ValidPortKey(k) {
			t.Errorf("%q must not be accepted", k)
		}
	}
}

// An annotation can name any interface; the reconciler ignores whatever is not a port key of the platform.
func TestAnnotationWithAHostInterfaceNameResolvesToNoPort(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	r := &NetworkAttachReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	att := NetAttachment{Iface: "x", Name: "eth0"}
	if got := r.resolveOVSPort(context.Background(), "ns", "pod", att); ValidPortKey(got) {
		t.Fatalf("resolved to %q", got)
	}
}

func TestIsPlatformPod(t *testing.T) {
	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	}
	for name, c := range map[string]struct {
		labels map[string]string
		ours   bool
	}{
		"device pod":        {map[string]string{names.LabelLab: "l", names.LabelDevice: "d"}, true},
		"vpn pod":           {map[string]string{names.LabelComponent: "vpn"}, true},
		"gateway pod":       {map[string]string{names.LabelComponent: "gateway"}, true},
		"old vpn pod":       {map[string]string{"app": "vpn"}, true},
		"someone else's":    {map[string]string{"app": "nginx"}, false},
		"no labels":         {nil, false},
		"lab label alone":   {map[string]string{names.LabelLab: "l"}, false},
		"unknown component": {map[string]string{names.LabelComponent: "x"}, false},
	} {
		if got := isPlatformPod(pod(c.labels)); got != c.ours {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// R-13: two groups with a connection of the same name and a switch of the same name no longer share patch ports.
func TestPatchPortNamesIncludeTheNamespace(t *testing.T) {
	a := patchPortName("lg-a", "conn", "sw")
	b := patchPortName("lg-b", "conn", "sw")
	if a == b {
		t.Fatal("a patch port is shared by two groups")
	}
	if len(a) > 15 {
		t.Fatalf("%q does not fit an interface name", a)
	}
	if patchPortName("lg-a", "conn", "sw") != a {
		t.Fatal("the name is stable")
	}
	if patchPortName("lg-a", "conn", "sw2") == a || patchPortName("lg-a", "conn2", "sw") == a {
		t.Fatal("the connection and the switch are part of the name")
	}
	if legacyPatchPortName("conn", "sw") == a {
		t.Fatal("the legacy name must differ from the new one, or it could not be told apart and replaced")
	}
}
