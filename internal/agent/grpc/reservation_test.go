package grpc

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// resNow is the fixed clock of the reservation tests.
var resNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// newReservationHandler is a Handler on fake clientsets with the fixed clock; objects are preloaded into the laboratory one.
func newReservationHandler(objs ...laboratoryv1alpha1.Tenant) *Handler {
	cs := fake.NewSimpleClientset()
	for i := range objs {
		_, _ = cs.LaboratoryV1alpha1().Tenants().Create(context.Background(), &objs[i], metav1.CreateOptions{})
	}
	h := NewHandler(cs, k8sfake.NewSimpleClientset(), nil)
	h.clock = func() time.Time { return resNow }
	return h
}

func unixMsOf(t time.Time) int64 { return t.UnixMilli() }

// resItem is a reservation item from now+from to now+to with cpu millicores and 1 GiB.
func resItem(id string, from, to time.Duration, cpu int64) *protobuf.ReservationItem {
	return &protobuf.ReservationItem{Id: id, FromUnixMs: unixMsOf(resNow.Add(from)), ToUnixMs: unixMsOf(resNow.Add(to)), CpuMillicores: cpu, MemoryBytes: 1 << 30}
}

func createRes(t *testing.T, h *Handler, ctx context.Context, items ...*protobuf.ReservationItem) *protobuf.BatchResult {
	t.Helper()
	res, err := h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Items: items})
	if err != nil {
		t.Fatalf("CreateReservations: %v", err)
	}
	return res
}

func TestCreateReservationStampsTenantAndName(t *testing.T) {
	h := newReservationHandler()
	ctx := asClient("backend-1")
	id := "Event Part/1" // not a DNS label: hashed name, original kept
	res, err := h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{
		Labels: map[string]string{"instance": "a"},
		Items:  []*protobuf.ReservationItem{{Id: id, FromUnixMs: unixMsOf(resNow.Add(time.Hour)) + 999, ToUnixMs: unixMsOf(resNow.Add(2 * time.Hour)), CpuMillicores: 1500, MemoryBytes: 2 << 30, Labels: map[string]string{"part": "p1"}}},
	})
	wantStates(t, res, err, stCreated)
	r, err := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, names.EncodeName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Spec.Tenant != "backend-1" || r.Labels[names.LabelTenant] != "backend-1" || r.Annotations[names.AnnotationID] != id {
		t.Fatalf("tenant and id: %+v %v %v", r.Spec, r.Labels, r.Annotations)
	}
	if r.Labels["instance"] != "a" || r.Labels["part"] != "p1" {
		t.Fatalf("request and item labels: %v", r.Labels)
	}
	if !r.Spec.From.Time.Equal(resNow.Add(time.Hour)) || !r.Spec.To.Time.Equal(resNow.Add(2*time.Hour)) {
		t.Fatalf("window kept to the second: %v %v", r.Spec.From, r.Spec.To)
	}
	if r.Spec.Resources.Cpu().MilliValue() != 1500 || r.Spec.Resources.Memory().Value() != 2<<30 {
		t.Fatalf("amount: %v", r.Spec.Resources)
	}
	list, err := h.ListReservations(ctx, &protobuf.ListRequest{})
	if err != nil || len(list.GetItems()) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	got := list.GetItems()[0]
	if got.GetId() != id || got.GetCpuMillicores() != 1500 || got.GetLabels()[names.LabelTenant] != "" || got.GetLabels()["part"] != "p1" {
		t.Fatalf("listed: %v", got)
	}
}

func TestCreateReservationIdempotent(t *testing.T) {
	h := newReservationHandler()
	ctx := asClient("backend-1")
	wantStates(t, createRes(t, h, ctx, resItem("r1", 0, time.Hour, 1000)), nil, stCreated)
	wantStates(t, createRes(t, h, ctx, resItem("r1", 0, time.Hour, 1000)), nil, stExists)
	res := createRes(t, h, ctx, resItem("r1", 0, 2*time.Hour, 1000))
	wantStates(t, res, nil, stFailed)
	if res.GetResults()[0].GetRetryable() {
		t.Fatal("a different spec is not retryable")
	}
	// another tenant cannot take the id and does not learn it exists
	other := createRes(t, h, asClient("backend-2"), resItem("r1", 0, time.Hour, 1000))
	wantStates(t, other, nil, stFailed)
	if got := other.GetResults()[0].GetError(); got != "Reservation r1: the id is not available" {
		t.Fatalf("taken: %q", got)
	}
}

func TestCreateReservationRejectsInvalidWindow(t *testing.T) {
	h := newReservationHandler()
	ctx := asClient("backend-1")
	res := createRes(t, h, ctx,
		resItem("backwards", time.Hour, 0, 1000),
		resItem("past", -2*time.Hour, -time.Hour, 1000),
		resItem("long", 0, 32*24*time.Hour, 1000),
		resItem("nocpu", 0, time.Hour, 0),
		&protobuf.ReservationItem{Id: "nomem", FromUnixMs: unixMsOf(resNow), ToUnixMs: unixMsOf(resNow.Add(time.Hour)), CpuMillicores: 1},
		resItem("ok", -time.Hour, time.Hour, 1000),
	)
	wantStates(t, res, nil, stFailed, stFailed, stFailed, stFailed, stFailed, stCreated)
	if _, err := h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Items: []*protobuf.ReservationItem{{Id: ""}}}); err == nil {
		t.Fatal("an empty id is INVALID_ARGUMENT")
	}
	if _, err := h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Items: []*protobuf.ReservationItem{
		{Id: "x", Labels: map[string]string{names.LabelReservation: "y"}}}}); err == nil {
		t.Fatal("a reserved label is INVALID_ARGUMENT")
	}
}

func TestUpdateReservationExtendsKeepsName(t *testing.T) {
	h := newReservationHandler()
	ctx := asClient("backend-1")
	wantStates(t, createRes(t, h, ctx, resItem("r1", -time.Hour, 3*time.Minute, 1000)), nil, stCreated)
	before, _ := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, "r1", metav1.GetOptions{})
	to := unixMsOf(resNow.Add(time.Hour))
	cpu := int64(2000)
	res, err := h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "r1", Changes: &protobuf.ReservationChanges{ToUnixMs: &to, CpuMillicores: &cpu}}}})
	wantStates(t, res, err, stUpdated)
	after, err := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, "r1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.UID != before.UID || after.Name != "r1" || !after.Spec.To.Time.Equal(resNow.Add(time.Hour)) || after.Spec.Resources.Cpu().MilliValue() != 2000 {
		t.Fatalf("extended in place: %+v", after.Spec)
	}
	past := unixMsOf(resNow)
	res, err = h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "r1", Changes: &protobuf.ReservationChanges{ToUnixMs: &past}}}})
	wantStates(t, res, err, stFailed)
	res, err = h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{{Id: "nope", Changes: &protobuf.ReservationChanges{ToUnixMs: &to}}}})
	wantStates(t, res, err, stNotFound)
}

func TestUpdateReservationFromOnlyWhilePending(t *testing.T) {
	h := newReservationHandler()
	ctx := asClient("backend-1")
	wantStates(t, createRes(t, h, ctx, resItem("future", time.Hour, 2*time.Hour, 1000), resItem("running", -time.Hour, time.Hour, 1000)), nil, stCreated, stCreated)
	from := unixMsOf(resNow.Add(30 * time.Minute))
	res, err := h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "future", Changes: &protobuf.ReservationChanges{FromUnixMs: &from}},
		{Id: "running", Changes: &protobuf.ReservationChanges{FromUnixMs: &from}},
	}})
	wantStates(t, res, err, stUpdated, stFailed)
	// the same from on a running one is no change
	same := unixMsOf(resNow.Add(-time.Hour))
	res, err = h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "running", Changes: &protobuf.ReservationChanges{FromUnixMs: &same}}}})
	wantStates(t, res, err, stUpdated)
}

func TestReservationOtherTenantInvisible(t *testing.T) {
	h := newReservationHandler()
	wantStates(t, createRes(t, h, asClient("backend-1"), resItem("r1", 0, time.Hour, 1000)), nil, stCreated)
	other := asClient("backend-2")
	list, err := h.ListReservations(other, &protobuf.ListRequest{})
	if err != nil || len(list.GetItems()) != 0 {
		t.Fatalf("another tenant lists nothing: %v %v", list, err)
	}
	list, err = h.ListReservations(other, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{Name: "r1"}}})
	if err != nil || len(list.GetItems()) != 0 {
		t.Fatalf("by id: %v %v", list, err)
	}
	to := unixMsOf(resNow.Add(2 * time.Hour))
	res, err := h.UpdateReservations(other, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{{Id: "r1", Changes: &protobuf.ReservationChanges{ToUnixMs: &to}}}})
	wantStates(t, res, err, stNotFound)
	del, err := h.DeleteReservations(other, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{Name: "r1"}}})
	wantStates(t, del, err, stNotFound)
	if _, err := h.cs.LaboratoryV1alpha1().Reservations().Get(context.Background(), "r1", metav1.GetOptions{}); err != nil {
		t.Fatalf("still there: %v", err)
	}
}

func quotaTenant(name, cpu string) laboratoryv1alpha1.Tenant {
	return laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: laboratoryv1alpha1.TenantSpec{Quota: &laboratoryv1alpha1.TenantQuota{CPU: cpu}}}
}

func TestReservationOverQuotaRefused(t *testing.T) {
	h := newReservationHandler(quotaTenant("backend-1", "10"))
	ctx := asClient("backend-1")
	res := createRes(t, h, ctx,
		resItem("a", 0, 2*time.Hour, 6000),
		resItem("b", time.Hour, 3*time.Hour, 6000),   // overlaps a: 12 CPU at once
		resItem("c", 2*time.Hour, 4*time.Hour, 6000), // after a: fits
	)
	wantStates(t, res, nil, stCreated, stFailed, stCreated)
	if r := res.GetResults()[1]; r.GetReason() != client.ReasonOverQuota || r.GetRetryable() {
		t.Fatalf("over quota: %v", r)
	}
	// lowering is fine; raising or extending is checked again where the windows overlap
	up, down := int64(5000), int64(1000)
	ures, err := h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "c", Changes: &protobuf.ReservationChanges{CpuMillicores: &down}},
	}})
	wantStates(t, ures, err, stUpdated)
	to := unixMsOf(resNow.Add(3 * time.Hour))
	ures, err = h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "a", Changes: &protobuf.ReservationChanges{ToUnixMs: &to, CpuMillicores: &up}},
	}})
	// a becomes 5 CPU until 3h, overlapping c (now 1 CPU) from 2h: 6 CPU at most, fits
	wantStates(t, ures, err, stUpdated)
	big := int64(9500)
	ures, err = h.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
		{Id: "a", Changes: &protobuf.ReservationChanges{CpuMillicores: &big}}, // 9.5 + 1 overlapping c: over 10
	}})
	wantStates(t, ures, err, stFailed)
	if ures.GetResults()[0].GetReason() != client.ReasonOverQuota {
		t.Fatalf("reason: %v", ures.GetResults()[0])
	}
}

func TestReservationNoQuotaNotChecked(t *testing.T) {
	h := newReservationHandler(laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "backend-1"}})
	ctx := asClient("backend-1")
	wantStates(t, createRes(t, h, ctx, resItem("a", 0, time.Hour, 600000), resItem("b", 0, time.Hour, 600000)), nil, stCreated, stCreated)
}

func TestDeleteReservationBySelector(t *testing.T) {
	h := newReservationHandler()
	ctx := asClient("backend-1")
	_, err := h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Labels: map[string]string{"instance": "a"}, Items: []*protobuf.ReservationItem{
		resItem("a1", 0, time.Hour, 1000), resItem("a2", 0, time.Hour, 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Labels: map[string]string{"instance": "b"}, Items: []*protobuf.ReservationItem{resItem("b1", 0, time.Hour, 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err := h.DeleteReservations(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "instance=a", ExpectedCount: &one}}); err == nil {
		t.Fatal("more matches than expected_count fail the call")
	}
	if _, err := h.DeleteReservations(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: names.LabelTenant + "=backend-1"}}); err == nil {
		t.Fatal("a selector on a reserved label is refused")
	}
	res, err := h.DeleteReservations(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "instance=a"}})
	wantStates(t, res, err, stDeleted, stDeleted)
	list, _ := h.ListReservations(ctx, &protobuf.ListRequest{})
	if len(list.GetItems()) != 1 || list.GetItems()[0].GetId() != "b1" {
		t.Fatalf("left: %v", list)
	}
}

func TestReservationStatusToProto(t *testing.T) {
	to := resNow.Add(4 * time.Minute)
	r := &laboratoryv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: "r"}}
	r.Spec.To = metav1.NewTime(to)
	notice := metav1.NewTime(resNow.Add(-time.Minute))
	r.Status.Phase, r.Status.NoticeAt = laboratoryv1alpha1.ReservationPhaseEnding, &notice
	r.Status.Conditions = []metav1.Condition{{Type: laboratoryv1alpha1.ReservationConditionEnding, Status: metav1.ConditionTrue, Reason: laboratoryv1alpha1.ReservationReasonWindowClosing}}
	st := reservationStatusToProto(r, 5*time.Minute)
	if st.GetPhase() != protobuf.ReservationPhase_RESERVATION_PHASE_ENDING || st.GetEndsAtUnixMs() != to.UnixMilli() || st.GetEndingReason() != "WindowClosing" ||
		st.GetNoticeAtUnixMs() != notice.UnixMilli() {
		t.Fatalf("window closing: %v", st)
	}
	// the tenant is gone: one notice after it was given, or `to` when sooner
	r.Spec.To = metav1.NewTime(resNow.Add(time.Hour))
	r.Status.Conditions[0].Reason = laboratoryv1alpha1.ReservationReasonTenantGone
	st = reservationStatusToProto(r, 5*time.Minute)
	if st.GetEndsAtUnixMs() != notice.Add(5*time.Minute).UnixMilli() || st.GetEndingReason() != "TenantGone" {
		t.Fatalf("tenant gone: %v", st)
	}
	// nothing announced
	r.Status.Phase, r.Status.NoticeAt, r.Status.Conditions = laboratoryv1alpha1.ReservationPhaseActive, nil, nil
	if st = reservationStatusToProto(r, 5*time.Minute); st.GetEndsAtUnixMs() != 0 || st.GetEndingReason() != "" || st.GetExceeded() {
		t.Fatalf("active: %v", st)
	}
}
