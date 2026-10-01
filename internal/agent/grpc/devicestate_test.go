package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestResetAndRescueDevice(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-state")
	devices := h.cs.LaboratoryV1alpha1().Devices("team-state")

	create := func(name, dev string, state *laboratoryv1alpha1.DeviceStateSpec) {
		t.Helper()
		_, err := devices.Create(ctx, &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-state"},
			Spec: laboratoryv1alpha1.DeviceSpec{
				LabRef: "ctf", Name: dev, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", State: state,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	create("ctf-web", "web", &laboratoryv1alpha1.DeviceStateSpec{Enabled: true})
	create("ctf-plain", "plain", nil)

	get := func(name string) *laboratoryv1alpha1.Device {
		t.Helper()
		d, err := devices.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	req := &protobuf.DeviceRequest{Namespace: "team-state", Lab: "ctf", Device: "web"}
	if _, err := h.ResetDevice(ctx, req); err != nil {
		t.Fatalf("ResetDevice: %v", err)
	}
	first := get("ctf-web").Spec.State.ResetToken
	if first == "" {
		t.Fatal("reset must set a token for the operator to act on")
	}
	if _, err := h.ResetDevice(ctx, req); err != nil {
		t.Fatal(err)
	}
	if second := get("ctf-web").Spec.State.ResetToken; second == "" || second == first {
		t.Fatalf("every reset needs a new token, got %q then %q", first, second)
	}
	if !get("ctf-web").Spec.State.Enabled {
		t.Fatal("reset must keep the policy")
	}

	rescue := &protobuf.RescueDeviceRequest{Namespace: "team-state", Lab: "ctf", Device: "web", Enable: true}
	if _, err := h.RescueDevice(ctx, rescue); err != nil {
		t.Fatalf("RescueDevice: %v", err)
	}
	if !get("ctf-web").Spec.State.Rescue {
		t.Fatal("rescue not enabled")
	}
	rescue.Enable = false
	if _, err := h.RescueDevice(ctx, rescue); err != nil {
		t.Fatal(err)
	}
	if get("ctf-web").Spec.State.Rescue {
		t.Fatal("rescue not disabled")
	}

	_, err := h.ResetDevice(ctx, &protobuf.DeviceRequest{Namespace: "team-state", Lab: "ctf", Device: "plain"})
	if status.Code(apiErrorToStatus(err)) != codes.FailedPrecondition {
		t.Fatalf("a device without state persistence cannot be reset, got %v", err)
	}
	_, err = h.RescueDevice(ctx, &protobuf.RescueDeviceRequest{Namespace: "team-state", Lab: "ctf", Device: "nope", Enable: true})
	if status.Code(apiErrorToStatus(err)) != codes.NotFound {
		t.Fatalf("got %v", err)
	}
	_, err = h.ResetDevice(ctx, &protobuf.DeviceRequest{Namespace: "team-state", Lab: "ctf"})
	if status.Code(apiErrorToStatus(err)) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
}

func TestLabStatusCarriesSnapshotInfo(t *testing.T) {
	at := metav1.NewTime(time.UnixMilli(1_700_000_000_000))
	lab := &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: "ctf", Namespace: "ns"},
		Status: laboratoryv1alpha1.LabStatus{Devices: []laboratoryv1alpha1.DeviceRef{
			{Name: "web", Ready: true, State: &laboratoryv1alpha1.DeviceStateInfo{LastSnapshotAt: &at, SizeBytes: 99, QuotaWarning: "quota exceeded", Rescue: true}},
			{Name: "db", Ready: true},
		}},
	}
	p := labToProto(lab)
	web, db := p.Status.Devices[0], p.Status.Devices[1]
	if web.Snapshot == nil || web.Snapshot.LastSnapshotUnixMs != 1_700_000_000_000 || web.Snapshot.SizeBytes != 99 ||
		web.Snapshot.Warning != "quota exceeded" || !web.Snapshot.Rescue || web.Snapshot.RestoredUnixMs != 0 {
		t.Fatalf("got %+v", web.Snapshot)
	}
	if db.Snapshot != nil {
		t.Fatal("a device without persistence reports no snapshot block")
	}
}

func TestImageWarningsReachTheAgentStatus(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: "ctf", Namespace: "ns"},
		Status:     laboratoryv1alpha1.LabStatus{ImageWarning: "image tags not pinned to a digest, pulled by tag: nginx:1"},
	}
	if got := labToProto(lab).Status.ImageWarning; got != lab.Status.ImageWarning {
		t.Fatalf("lab warning %q", got)
	}
	g := &laboratoryv1alpha1.LabGroup{Status: laboratoryv1alpha1.LabGroupStatus{ImageWarning: "images not pinned to a digest, pulled by tag: lab:v1"}}
	if got := labGroupToProto(g).Status.ImageWarning; got != g.Status.ImageWarning {
		t.Fatalf("group warning %q", got)
	}
	if labToProto(&laboratoryv1alpha1.Lab{}).Status.ImageWarning != "" {
		t.Fatal("no warning unless there is one")
	}
}
