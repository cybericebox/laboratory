package nodeagent

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func TestGroupComponent(t *testing.T) {
	pod := func(labels map[string]string) *corev1.Pod { return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: labels}} }
	for name, tc := range map[string]struct {
		labels map[string]string
		want   string
	}{
		"vpn by component":     {map[string]string{names.LabelComponent: "vpn"}, "vpn"},
		"gateway by component": {map[string]string{names.LabelComponent: "gateway"}, "gateway"},
		"device":               {map[string]string{names.LabelLab: "l", names.LabelDevice: "d", "app": "vpn"}, ""},
		"other pod":            {map[string]string{"app": "web"}, ""},
	} {
		if got := GroupComponent(pod(tc.labels)); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// B-4: the lab interfaces of a group's VPN and gateway pod come from its LabVPN and LabGateway objects, and a lab that is gone is detached.
func TestGroupPodAttachmentsFollowTheLabObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	vpn := func(ns, lab string, n uint) *laboratoryv1alpha1.LabVPN {
		return &laboratoryv1alpha1.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName(lab), Namespace: ns}, Spec: laboratoryv1alpha1.LabVPNSpec{LabName: lab, NetworkIndex: n}}
	}
	gw := func(ns, lab string, n uint) *laboratoryv1alpha1.LabGateway {
		return &laboratoryv1alpha1.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: names.LabGatewayObjectName(lab), Namespace: ns}, Spec: laboratoryv1alpha1.LabGatewaySpec{LabName: lab, NetworkIndex: n}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		vpn("ns-a", "l1", 3), vpn("ns-a", "l2", 1), vpn("ns-b", "l9", 7), gw("ns-a", "l1", 2),
	).Build()
	ctx := context.Background()

	got, err := GroupPodAttachments(ctx, c, "ns-a", names.ComponentVPN)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lab1@" + names.VPNHostPortKey("ns-a", 1), "lab3@" + names.VPNHostPortKey("ns-a", 3)}
	var have []string
	for _, a := range got {
		have = append(have, a.Iface+"@"+a.Name)
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("VPN attachments of ns-a: %v, want %v", have, want)
	}
	gws, _ := GroupPodAttachments(ctx, c, "ns-a", names.ComponentGateway)
	if len(gws) != 1 || gws[0].Iface != "lab2" || gws[0].Name != names.GWHostPortKey("ns-a", 2) {
		t.Fatalf("gateway attachments: %+v", gws)
	}
	if other, _ := GroupPodAttachments(ctx, c, "ns-c", names.ComponentVPN); len(other) != 0 {
		t.Fatalf("another namespace has none: %+v", other)
	}

	// the leg of lab index 3 is on the node, and so is a leg of a lab that is gone (index 5) and a port of another group
	present := map[string]bool{
		names.VPNHostPortKey("ns-a", 1): true, names.VPNHostPortKey("ns-a", 3): true,
		names.VPNHostPortKey("ns-a", 5): true, names.VPNHostPortKey("ns-b", 7): true,
		names.GWHostPortKey("ns-a", 2): true,
	}
	if stale := StaleGroupPorts("ns-a", names.ComponentVPN, got, present); !reflect.DeepEqual(stale, []string{names.VPNHostPortKey("ns-a", 5)}) {
		t.Fatalf("stale VPN legs: %v", stale)
	}
	if all := GroupPortsPresent("ns-a", names.ComponentVPN, present); len(all) != 3 {
		t.Fatalf("all VPN legs of ns-a: %v", all)
	}
	if stale := StaleGroupPorts("ns-a", names.ComponentGateway, gws, present); len(stale) != 0 {
		t.Fatalf("the gateway has no stale leg: %v", stale)
	}
}
