package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestDeviceCallsEndToEnd(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-state", "team-state", nil)
	devices := h.cs.LaboratoryV1alpha1().Devices("team-state")

	create := func(lab, dev string, state *laboratoryv1alpha1.DeviceStateSpec) {
		t.Helper()
		_, err := devices.Create(ctx, &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: lab + "-" + dev, Namespace: "team-state"},
			Spec: laboratoryv1alpha1.DeviceSpec{
				LabRef: lab, Name: dev, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", State: state,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.cs.LaboratoryV1alpha1().Labs("team-state").Create(ctx, &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Name: lab, Namespace: "team-state", Labels: map[string]string{"round": "1"}},
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	create("ctf", "web", &laboratoryv1alpha1.DeviceStateSpec{Enabled: true})
	create("ctf", "plain", nil)
	create("ctf2", "web", &laboratoryv1alpha1.DeviceStateSpec{Enabled: true})

	get := func(name string) *laboratoryv1alpha1.Device {
		t.Helper()
		d, err := devices.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	web := func(lab string) *protobuf.DevicesRequest {
		return &protobuf.DevicesRequest{Items: []*protobuf.ItemRef{{LabGroup: "team-state", Lab: lab, Name: "web"}}}
	}

	res, err := h.ResetDevices(ctx, web("ctf"))
	wantStates(t, res, err, stUpdated)
	first := get("ctf-web").Spec.State.ResetToken
	if first == "" {
		t.Fatal("reset must set a token for the operator to act on")
	}
	res, err = h.ResetDevices(ctx, web("ctf"))
	wantStates(t, res, err, stUpdated)
	if second := get("ctf-web").Spec.State.ResetToken; second == "" || second == first {
		t.Fatalf("every reset needs a new token, got %q then %q", first, second)
	}
	if !get("ctf-web").Spec.State.Enabled {
		t.Fatal("reset must keep the policy")
	}

	res, err = h.RescueDevices(ctx, &protobuf.RescueDevicesRequest{Devices: web("ctf"), Enable: true})
	wantStates(t, res, err, stUpdated)
	if !get("ctf-web").Spec.State.Rescue {
		t.Fatal("rescue not enabled")
	}
	res, err = h.RescueDevices(ctx, &protobuf.RescueDevicesRequest{Devices: web("ctf")})
	wantStates(t, res, err, stUpdated)
	if get("ctf-web").Spec.State.Rescue {
		t.Fatal("rescue not disabled")
	}

	// Per-item failures do not fail the call.
	res, err = h.ResetDevices(ctx, &protobuf.DevicesRequest{Items: []*protobuf.ItemRef{
		{LabGroup: "team-state", Lab: "ctf", Name: "plain"},
		{LabGroup: "team-state", Lab: "ctf", Name: "nope"},
		{LabGroup: "no-group", Lab: "ctf", Name: "web"},
	}})
	wantStates(t, res, err, stFailed, stNotFound, stNotFound)
	if !strings.Contains(res.Results[0].Error, "no state persistence") {
		t.Fatalf("got %v", res.Results[0])
	}
	// A malformed item fails the whole call.
	if _, err = h.ResetDevices(ctx, &protobuf.DevicesRequest{Items: []*protobuf.ItemRef{{LabGroup: "team-state", Lab: "ctf"}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}

	// Selector over labs plus a device name, with the guard.
	sel := func(expected int64) *protobuf.DevicesRequest {
		return &protobuf.DevicesRequest{BySelector: &protobuf.DeviceSelector{Selector: "round=1", Device: "web", ExpectedCount: proto.Int64(expected)}}
	}
	if _, err = h.ResetDevices(ctx, sel(1)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("guard: %v", err)
	}
	res, err = h.ResetDevices(ctx, sel(2))
	wantStates(t, res, err, stUpdated, stUpdated)
	if res.Results[0].Ref.Lab != "ctf" || res.Results[1].Ref.Lab != "ctf2" || get("ctf2-web").Spec.State.ResetToken == "" {
		t.Fatalf("selector results: %v", res.Results)
	}
	if _, err = h.ResetDevices(ctx, &protobuf.DevicesRequest{BySelector: &protobuf.DeviceSelector{Selector: "round=1"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a selector needs a device: %v", err)
	}
	if _, err = h.ResetDevices(ctx, &protobuf.DevicesRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no target: %v", err)
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
