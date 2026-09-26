package grpc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestCreateAndGetLabGroup(t *testing.T) {
	h, _ := newTestHandler(t) // sets up envtest + versioned clientset; see handler_test_setup.go
	ctx := context.Background()

	_, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "team-x"})
	if err != nil {
		t.Fatalf("CreateLabGroup: %v", err)
	}
	got, err := h.GetLabGroup(ctx, &protobuf.IDRequest{Name: "team-x"})
	if err != nil {
		t.Fatalf("GetLabGroup: %v", err)
	}
	if got.Name != "team-x" {
		t.Errorf("got %q", got.Name)
	}
	_ = metav1.Now
}

func TestSetLabGroupSuspendedPersistsOnlyDesiredState(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()

	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "team-suspend"}); err != nil {
		t.Fatalf("CreateLabGroup: %v", err)
	}

	for _, suspended := range []bool{true, true, false} {
		if _, err := h.SetLabGroupSuspended(ctx, &protobuf.LabGroupSuspendRequest{
			Name:      "team-suspend",
			Suspended: suspended,
		}); err != nil {
			t.Fatalf("SetLabGroupSuspended(%t): %v", suspended, err)
		}

		group, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "team-suspend", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get LabGroup: %v", err)
		}
		if group.Spec.Suspended != suspended {
			t.Fatalf("spec.suspended = %t, want %t", group.Spec.Suspended, suspended)
		}
		if group.Status.Phase != "" || group.Status.Namespace != "" || group.Status.VPN.Registered {
			t.Fatalf("set operation changed observed status: %#v", group.Status)
		}
	}
}

func TestSetLabGroupVPNDisabledPreservesGatewayState(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "team-vpn-toggle"}); err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{true, true, false} {
		if _, err := h.SetLabGroupVPNDisabled(ctx, &protobuf.LabGroupVPNDisabledRequest{Name: "team-vpn-toggle", Disabled: disabled}); err != nil {
			t.Fatal(err)
		}
		group, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "team-vpn-toggle", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if group.Spec.VPN.Disabled != disabled || group.Spec.VPN.ProbeWhileSuspended || group.Spec.Suspended {
			t.Fatalf("unexpected group spec: %+v", group.Spec)
		}
	}
	if _, err := h.SetLabGroupVPNDisabled(ctx, &protobuf.LabGroupVPNDisabledRequest{Name: "team-vpn-toggle", ProbeWhileSuspended: true}); err != nil {
		t.Fatal(err)
	}
	group, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "team-vpn-toggle", metav1.GetOptions{})
	if err != nil || !group.Spec.VPN.ProbeWhileSuspended || group.Spec.VPN.Disabled || group.Spec.Suspended {
		t.Fatalf("probe-only mode not stored independently: %+v %v", group, err)
	}
}
