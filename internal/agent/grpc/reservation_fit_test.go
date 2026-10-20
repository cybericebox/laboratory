package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// newFitHandler is an envtest Handler that requires reservations, as production does.
func newFitHandler(t *testing.T) (*Handler, kubernetes.Interface) {
	t.Helper()
	h, k8s := newTestHandler(t)
	h.reservationsOptional = false
	return h, k8s
}

// reserve creates a reservation of the default tenant from an hour ago (or from `from` ahead) for two hours.
func reserve(t *testing.T, h *Handler, id string, cpu int64, from time.Duration) {
	t.Helper()
	now := time.Now()
	res, err := h.CreateReservations(context.Background(), &protobuf.CreateReservationsRequest{Items: []*protobuf.ReservationItem{{
		Id: id, FromUnixMs: now.Add(from).UnixMilli(), ToUnixMs: now.Add(from + 2*time.Hour).UnixMilli(), CpuMillicores: cpu, MemoryBytes: 64 << 30}}})
	wantStates(t, res, err, stCreated)
}

// reservedGroup creates a group under a reservation and plays the operator (namespace, status.namespace).
func reservedGroup(t *testing.T, h *Handler, k8s kubernetes.Interface, name, reservation string) {
	t.Helper()
	ctx := context.Background()
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: name, ReservationId: reservation}}})
	wantStates(t, res, err, stCreated)
	mustNamespace(t, k8s, name)
	g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g.Status.Namespace = name
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(ctx, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// cpuSpec is a lab spec with one container device that runs with the given cpu (and 64Mi).
func cpuSpec(cpu string) []byte {
	spec := laboratoryv1alpha1.LabSpec{}
	spec.VPN.Enabled = true
	spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{Name: "box", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx",
		Resources: &laboratoryv1alpha1.DeviceResources{CPULimit: cpu, MemoryLimit: "64Mi"}}}
	raw, _ := json.Marshal(spec)
	return raw
}

func wantReason(t *testing.T, r *protobuf.ItemResult, reason string, retryable bool) {
	t.Helper()
	if r.GetState() != stFailed || r.GetReason() != reason || r.GetRetryable() != retryable {
		t.Fatalf("want FAILED %s (retryable %t), got %v", reason, retryable, r)
	}
}

// The reservation rules of CreateLabGroups, CreateLabs and the attach, on one API server (each case under its own names).
func TestReservationFitRules(t *testing.T) {
	h, k8s := newFitHandler(t)
	for i, c := range []struct {
		name string
		run  func(*testing.T, *Handler, kubernetes.Interface, func(string) string)
	}{
		{"CreateLabGroupRequiresReservation", fitCreateLabGroupRequiresReservation},
		{"CreateLabRequiresReservation", fitCreateLabRequiresReservation},
		{"CreateLabsRefusedWhenReservationFull", fitCreateLabsRefusedWhenReservationFull},
		{"StoppedLabsDoNotCountInFit", fitStoppedLabsDoNotCountInFit},
		{"GroupSizesCountInFit", fitGroupSizesCountInFit},
		{"EndedReservationRefused", fitEndedReservationRefused},
		{"PendingReservationAccepted", fitPendingReservationAccepted},
		{"DeployGroupUnderOneReservation", fitDeployGroupUnderOneReservation},
		{"ReservationLabelFromFieldNotLabels", fitReservationLabelFromFieldNotLabels},
		{"AttachReservationToRunningLab", fitAttachReservationToRunningLab},
		{"AttachMoveRefused", fitAttachMoveRefused},
		{"OtherTenantReservationNotFound", fitOtherTenantReservationNotFound},
	} {
		prefix := fmt.Sprintf("c%d-", i)
		t.Run(c.name, func(t *testing.T) { c.run(t, h, k8s, func(s string) string { return prefix + s }) })
	}
}

func fitCreateLabGroupRequiresReservation(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	res, err := h.CreateLabGroups(context.Background(), &protobuf.CreateLabGroupsRequest{Items: groupItems(n("g1"))})
	wantStates(t, res, err, stFailed)
	wantReason(t, res.GetResults()[0], client.ReasonReservationRequired, false)
	if res.GetResults()[0].GetError() != "a reservation id is required" {
		t.Fatalf("text: %q", res.GetResults()[0].GetError())
	}
}

func fitCreateLabRequiresReservation(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	reserve(t, h, n("r1"), 4000, -time.Hour)
	reservedGroup(t, h, k8s, n("g1"), n("r1"))
	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: cpuSpec("100m")}},
		Items:    []*protobuf.LabItem{{LabGroup: n("g1"), Name: n("l1"), VariantId: "v"}, {LabGroup: n("g1"), Name: n("l2"), VariantId: "v", ReservationId: n("r1")}},
	})
	wantStates(t, res, err, stFailed, stCreated)
	wantReason(t, res.GetResults()[0], client.ReasonReservationRequired, false)
	l2, err := h.cs.LaboratoryV1alpha1().Labs(n("g1")).Get(context.Background(), n("l2"), metav1.GetOptions{})
	if err != nil || l2.Labels[names.LabelReservation] != n("r1") {
		t.Fatalf("the lab carries its reservation label: %v %v", l2, err)
	}
}

func fitCreateLabsRefusedWhenReservationFull(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	reserve(t, h, n("groups"), 4000, -time.Hour)
	reserve(t, h, n("r1"), 1000, -time.Hour)
	reservedGroup(t, h, k8s, n("g1"), n("groups"))
	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: cpuSpec("600m")}},
		Items: []*protobuf.LabItem{
			{LabGroup: n("g1"), Name: n("l1"), VariantId: "v", ReservationId: n("r1")},
			{LabGroup: n("g1"), Name: n("l2"), VariantId: "v", ReservationId: n("r1")},
		},
	})
	wantStates(t, res, err, stCreated, stFailed)
	wantReason(t, res.GetResults()[1], client.ReasonReservationFull, true)
	if got := res.GetResults()[1].GetError(); got != "not enough room in this reservation (needs cpu 600m memory 67108864, left cpu 400m memory 68652367872)" {
		t.Fatalf("text: %q", got)
	}
	// resending the lab that exists is idempotent: no fit check
	res, err = h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: cpuSpec("600m")}},
		Items:    []*protobuf.LabItem{{LabGroup: n("g1"), Name: n("l1"), VariantId: "v", ReservationId: n("r1")}},
	})
	wantStates(t, res, err, stExists)
}

func fitStoppedLabsDoNotCountInFit(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	ctx := context.Background()
	reserve(t, h, n("groups"), 4000, -time.Hour)
	reserve(t, h, n("r1"), 1000, -time.Hour)
	reservedGroup(t, h, k8s, n("g1"), n("groups"))
	variants := []*protobuf.LabVariant{{VariantId: "v", SpecJson: cpuSpec("600m")}}
	res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: variants, Items: []*protobuf.LabItem{{LabGroup: n("g1"), Name: n("l1"), VariantId: "v", ReservationId: n("r1")}}})
	wantStates(t, res, err, stCreated)
	l1, err := h.cs.LaboratoryV1alpha1().Labs(n("g1")).Get(ctx, n("l1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l1.Spec.Lifecycle = &laboratoryv1alpha1.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1, SnapshotMode: "Skip"}
	if _, err := h.cs.LaboratoryV1alpha1().Labs(n("g1")).Update(ctx, l1, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err = h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: variants, Items: []*protobuf.LabItem{{LabGroup: n("g1"), Name: n("l2"), VariantId: "v", ReservationId: n("r1")}}})
	wantStates(t, res, err, stCreated)
}

func fitGroupSizesCountInFit(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	// the chart default: VPN 100m and gateway 10m
	reserve(t, h, n("r1"), 200, -time.Hour)
	res, err := h.CreateLabGroups(context.Background(), &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{
		{Name: n("g1"), ReservationId: n("r1")}, {Name: n("g2"), ReservationId: n("r1")}, {Name: n("g3"), ReservationId: n("r1"), VpnDisabled: true}}})
	wantStates(t, res, err, stCreated, stFailed, stCreated)
	wantReason(t, res.GetResults()[1], client.ReasonReservationFull, true)
	want := h.groupNeedOf(&laboratoryv1alpha1.LabGroup{})
	if d := (grouppods.Config{}).Overhead(); want.CPU != 110 || want.CPU != d.CPU || want.Memory != d.Memory {
		t.Fatalf("a group without sizes counts the chart default: %+v %+v", want, d)
	}
}

func fitEndedReservationRefused(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	ctx := context.Background()
	reserve(t, h, n("r1"), 4000, -time.Hour)
	r, err := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, n("r1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r.Status.Phase = laboratoryv1alpha1.ReservationPhaseEnded
	if _, err := h.cs.LaboratoryV1alpha1().Reservations().UpdateStatus(ctx, r, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g1"), ReservationId: n("r1")}, {Name: n("g2"), ReservationId: n("nope")}}})
	wantStates(t, res, err, stFailed, stFailed)
	wantReason(t, res.GetResults()[0], client.ReasonReservationEnded, false)
	wantReason(t, res.GetResults()[1], client.ReasonReservationNotFound, false)
}

func fitPendingReservationAccepted(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	reserve(t, h, n("later"), 4000, time.Hour)
	res, err := h.CreateLabGroups(context.Background(), &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g1"), ReservationId: n("later")}}})
	wantStates(t, res, err, stCreated)
}

func fitDeployGroupUnderOneReservation(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	ctx := context.Background()
	reserve(t, h, n("r1"), 4000, -time.Hour)
	reserve(t, h, n("r2"), 4000, -time.Hour)
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{
		{Name: n("g1"), ReservationId: n("r1"), DeployGroup: n("stage")}, {Name: n("g2"), ReservationId: n("r2"), DeployGroup: n("stage")}}})
	wantStates(t, res, err, stCreated, stFailed)
	wantReason(t, res.GetResults()[1], client.ReasonReservationMismatch, false)
	// against what runs in the cluster
	res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{
		{Name: n("g3"), ReservationId: n("r2"), DeployGroup: n("stage")}, {Name: n("g4"), ReservationId: n("r1"), DeployGroup: n("stage")}, {Name: n("g5"), ReservationId: n("r2"), DeployGroup: n("other")}}})
	wantStates(t, res, err, stFailed, stCreated, stCreated)
	wantReason(t, res.GetResults()[0], client.ReasonReservationMismatch, false)
}

func fitReservationLabelFromFieldNotLabels(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	ctx := context.Background()
	reserve(t, h, n("Event Part 1"), 4000, -time.Hour)
	_, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{
		{Name: n("g1"), ReservationId: n("Event Part 1"), Labels: map[string]string{names.LabelReservation: "x"}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a client label under the reserved prefix is INVALID_ARGUMENT: %v", err)
	}
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g1"), ReservationId: n("Event Part 1")}}})
	wantStates(t, res, err, stCreated)
	g, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, n("g1"), metav1.GetOptions{})
	if got := g.Labels[names.LabelReservation]; got != names.EncodeName(n("Event Part 1")) {
		t.Fatalf("the label is the reservation's CR name: %q", got)
	}
}

func fitAttachReservationToRunningLab(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	h.reservationsOptional = true // the objects come from before reservations: created without one
	defer func() { h.reservationsOptional = false }()
	ctx := context.Background()
	readyGroup(t, h, k8s, n("g1"), n("g1"), nil)
	res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: cpuSpec("100m")}},
		Items:    []*protobuf.LabItem{{LabGroup: n("g1"), Name: n("l1"), VariantId: "v"}},
	})
	wantStates(t, res, err, stCreated)
	h.reservationsOptional = false
	reserve(t, h, n("r1"), 4000, -time.Hour)
	before, _ := h.cs.LaboratoryV1alpha1().Labs(n("g1")).Get(ctx, n("l1"), metav1.GetOptions{})
	groupBefore, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, n("g1"), metav1.GetOptions{})

	ures, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{{Name: n("g1"), Changes: &protobuf.LabGroupChanges{ReservationId: proto.String(n("r1"))}}}})
	wantStates(t, ures, err, stUpdated)
	lres, err := h.UpdateLabs(ctx, &protobuf.UpdateLabsRequest{Items: []*protobuf.UpdateLabItem{{LabGroup: n("g1"), Name: n("l1"), Changes: &protobuf.LabChanges{ReservationId: proto.String(n("r1"))}}}})
	wantStates(t, lres, err, stUpdated)

	after, _ := h.cs.LaboratoryV1alpha1().Labs(n("g1")).Get(ctx, n("l1"), metav1.GetOptions{})
	groupAfter, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, n("g1"), metav1.GetOptions{})
	if after.Labels[names.LabelReservation] != n("r1") || groupAfter.Labels[names.LabelReservation] != n("r1") {
		t.Fatalf("labels: %v %v", after.Labels, groupAfter.Labels)
	}
	if after.UID != before.UID || after.Generation != before.Generation || !equality.Semantic.DeepEqual(after.Spec, before.Spec) {
		t.Fatalf("the lab is the same object with the same spec: gen %d -> %d", before.Generation, after.Generation)
	}
	if groupAfter.Generation != groupBefore.Generation || !equality.Semantic.DeepEqual(groupAfter.Spec, groupBefore.Spec) {
		t.Fatalf("the group's spec is untouched: gen %d -> %d", groupBefore.Generation, groupAfter.Generation)
	}
}

func fitAttachMoveRefused(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	ctx := context.Background()
	reserve(t, h, n("r1"), 4000, -time.Hour)
	reserve(t, h, n("r2"), 4000, -time.Hour)
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g1"), ReservationId: n("r1")}}})
	wantStates(t, res, err, stCreated)
	attach := func(id string) *protobuf.BatchResult {
		t.Helper()
		res, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{{Name: n("g1"), Changes: &protobuf.LabGroupChanges{ReservationId: proto.String(id)}}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	wantReason(t, attach(n("r2")).GetResults()[0], client.ReasonReservationMismatch, false)
	wantStates(t, attach(n("r1")), nil, stUpdated) // the same one: nothing to do
	// a create resend that names another reservation does not move it either
	res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g1"), ReservationId: n("r2")}}})
	wantStates(t, res, err, stFailed)
	wantReason(t, res.GetResults()[0], client.ReasonReservationMismatch, false)
	g, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, n("g1"), metav1.GetOptions{})
	if g.Labels[names.LabelReservation] != n("r1") {
		t.Fatalf("still under r1: %v", g.Labels)
	}
}

func fitOtherTenantReservationNotFound(t *testing.T, h *Handler, k8s kubernetes.Interface, n func(string) string) {
	ctx := context.Background()
	// another tenant's reservation, under its own id
	now := time.Now()
	res, err := h.CreateReservations(asClient("other"), &protobuf.CreateReservationsRequest{Items: []*protobuf.ReservationItem{{
		Id: n("theirs"), FromUnixMs: now.Add(-time.Hour).UnixMilli(), ToUnixMs: now.Add(time.Hour).UnixMilli(), CpuMillicores: 4000, MemoryBytes: 1 << 30}}})
	wantStates(t, res, err, stCreated)
	reserve(t, h, n("mine"), 4000, -time.Hour)

	res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g1"), ReservationId: n("theirs")}}})
	wantStates(t, res, err, stFailed)
	wantReason(t, res.GetResults()[0], client.ReasonReservationNotFound, false)

	reservedGroup(t, h, k8s, n("g2"), n("mine"))
	res, err = h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: cpuSpec("100m")}},
		Items:    []*protobuf.LabItem{{LabGroup: n("g2"), Name: n("l1"), VariantId: "v", ReservationId: n("theirs")}},
	})
	wantStates(t, res, err, stFailed)
	wantReason(t, res.GetResults()[0], client.ReasonReservationNotFound, false)

	// attach: a group that runs under none cannot be put under another tenant's reservation
	h.reservationsOptional = true
	res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: n("g3")}}})
	h.reservationsOptional = false
	wantStates(t, res, err, stCreated)
	res, err = h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{
		{Name: n("g3"), Changes: &protobuf.LabGroupChanges{ReservationId: proto.String(n("theirs"))}}}})
	wantStates(t, res, err, stFailed)
	wantReason(t, res.GetResults()[0], client.ReasonReservationNotFound, false)
	g, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, n("g3"), metav1.GetOptions{})
	if _, has := names.ReservationOf(g.Labels); has {
		t.Fatalf("no label written: %v", g.Labels)
	}
}

// The CPU limit above the request is a burst: the lab needs the request.
func TestLabNeedUsesTheOperatorFunction(t *testing.T) {
	d := limits.DeviceDefaults{CPU: "100m", Memory: "128Mi", MaxCPU: "2", MaxMemory: "4Gi"}
	spec := laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "a", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "b", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: &laboratoryv1alpha1.DeviceResources{CPURequest: "250m", CPULimit: "500m", MemoryRequest: "1Gi"}},
		{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeHub},
	}}
	if got := labNeed(&spec, d); got.CPU != 350 || got.Memory != 128<<20+1<<30 {
		t.Fatalf("need: %+v", got)
	}
}
