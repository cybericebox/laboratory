package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// observeOnce is one observation of the platform with a cache the test keeps (for the usage coalescing across polls).
func observeOnce(t *testing.T, h *Handler, c *monCache) *monState {
	t.Helper()
	st, err := h.observe(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func monCacheFor(t *testing.T, h *Handler) *monCache {
	t.Helper()
	c, err := newMonCache(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.stop)
	return c
}

// waitObserved polls the cache until the observation satisfies ok.
func waitObserved(t *testing.T, h *Handler, c *monCache, ok func(*monState) bool) *monState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := observeOnce(t, h, c)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("not observed: %v", st.update)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reservationsOf(u *protobuf.MonitoringUpdate) map[string]*protobuf.Reservation {
	out := map[string]*protobuf.Reservation{}
	for _, r := range u.GetReservations() {
		out[r.GetId()] = r
	}
	return out
}

func TestSnapshotCarriesReservations(t *testing.T) {
	h := newReservationHandler()
	createRes(t, h, context.Background(), resItem("mine", 0, time.Hour, 1000))
	createRes(t, h, asClient("backend-2"), resItem("theirs", 0, time.Hour, 1000))
	c := monCacheFor(t, h)
	st := waitObserved(t, h, c, func(s *monState) bool { return len(s.update.GetReservations()) == 2 })
	filter, err := newSelectorFilter("", names.DefaultTenant)
	if err != nil {
		t.Fatal(err)
	}
	got := reservationsOf(filter.snapshot(st))
	if len(got) != 1 || got["mine"] == nil || got["mine"].GetCpuMillicores() != 1000 {
		t.Fatalf("the snapshot carries the tenant's reservations: %v", got)
	}
}

func TestOtherTenantReservationsHidden(t *testing.T) {
	h := newReservationHandler()
	_, err := h.CreateReservations(asClient("backend-2"), &protobuf.CreateReservationsRequest{Labels: map[string]string{"instance": "a"},
		Items: []*protobuf.ReservationItem{resItem("theirs", 0, time.Hour, 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	c := monCacheFor(t, h)
	st := waitObserved(t, h, c, func(s *monState) bool { return len(s.update.GetReservations()) == 1 })
	for _, tc := range []struct{ tenant, selector string }{{names.DefaultTenant, ""}, {names.DefaultTenant, "instance=a"}, {"backend-3", ""}} {
		f, err := newSelectorFilter(tc.selector, tc.tenant)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.snapshot(st).GetReservations(); len(got) != 0 {
			t.Fatalf("%+v sees %v", tc, got)
		}
	}
	f, _ := newSelectorFilter("instance=a", "backend-2")
	if got := f.snapshot(st).GetReservations(); len(got) != 1 {
		t.Fatalf("the owner with its selector sees it: %v", got)
	}
	f, _ = newSelectorFilter("instance=b", "backend-2")
	if got := f.snapshot(st).GetReservations(); len(got) != 0 {
		t.Fatalf("the selector applies to reservations: %v", got)
	}
}

func setReservationStatus(t *testing.T, h *Handler, name string, edit func(*laboratoryv1alpha1.ReservationStatus)) {
	t.Helper()
	r, err := h.cs.LaboratoryV1alpha1().Reservations().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	edit(&r.Status)
	if _, err := h.cs.LaboratoryV1alpha1().Reservations().UpdateStatus(context.Background(), r, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestReservationDeltaOnPhaseChange(t *testing.T) {
	h := newReservationHandler()
	createRes(t, h, context.Background(), resItem("r1", 0, time.Hour, 1000), resItem("r2", 0, time.Hour, 1000))
	c := monCacheFor(t, h)
	setReservationStatus(t, h, "r1", func(s *laboratoryv1alpha1.ReservationStatus) { s.Phase = laboratoryv1alpha1.ReservationPhaseActive })
	prev := waitObserved(t, h, c, func(s *monState) bool {
		return reservationsOf(s.update)["r1"].GetStatus().GetPhase() == protobuf.ReservationPhase_RESERVATION_PHASE_ACTIVE
	})
	if _, changed := monitoringDelta(prev.update, observeOnce(t, h, c).update); changed {
		t.Fatal("nothing changed, nothing is sent")
	}
	notice := metav1.NewTime(resNow)
	setReservationStatus(t, h, "r1", func(s *laboratoryv1alpha1.ReservationStatus) {
		s.Phase, s.NoticeAt = laboratoryv1alpha1.ReservationPhaseEnding, &notice
		s.Conditions = []metav1.Condition{{Type: laboratoryv1alpha1.ReservationConditionEnding, Status: metav1.ConditionTrue, Reason: laboratoryv1alpha1.ReservationReasonWindowClosing,
			Message: "m", LastTransitionTime: notice}}
	})
	next := waitObserved(t, h, c, func(s *monState) bool {
		return reservationsOf(s.update)["r1"].GetStatus().GetPhase() == protobuf.ReservationPhase_RESERVATION_PHASE_ENDING
	})
	delta, changed := monitoringDelta(prev.update, next.update)
	if !changed || len(delta.GetReservations()) != 1 || delta.GetReservations()[0].GetId() != "r1" {
		t.Fatalf("only the changed reservation goes out: %v", delta)
	}
	st := delta.GetReservations()[0].GetStatus()
	if st.GetEndsAtUnixMs() != resNow.Add(time.Hour).UnixMilli() || st.GetEndingReason() != "WindowClosing" || st.GetNoticeAtUnixMs() != resNow.UnixMilli() {
		t.Fatalf("the ending notice: %v", st)
	}
}

func TestUsedChangesCoalesced(t *testing.T) {
	c := &monCache{shown: map[string]shownUsage{}}
	at := resNow
	st := func(cpu int64, usedAt time.Time) *protobuf.ReservationStatus {
		return &protobuf.ReservationStatus{UsedCpuMillicores: cpu, UsedMemoryBytes: 1, Groups: 1, UsedAtUnixMs: usedAt.UnixMilli()}
	}
	first := st(100, at)
	c.coalesceUsage("r", first, at)
	if first.GetUsedCpuMillicores() != 100 {
		t.Fatal("the first observation is shown as it is")
	}
	// a change 10 s after the last shown one is held back
	held := st(200, at.Add(10*time.Second))
	c.coalesceUsage("r", held, at.Add(10*time.Second))
	if !proto.Equal(held, first) {
		t.Fatalf("held: %v", held)
	}
	// and shown once 30 s passed since the last shown change
	shown := st(300, at.Add(30*time.Second))
	c.coalesceUsage("r", shown, at.Add(30*time.Second))
	if shown.GetUsedCpuMillicores() != 300 {
		t.Fatalf("shown: %v", shown)
	}
	again := st(400, at.Add(40*time.Second))
	c.coalesceUsage("r", again, at.Add(40*time.Second))
	if again.GetUsedCpuMillicores() != 300 {
		t.Fatalf("at most one change per 30 s: %v", again)
	}
	// the heartbeat alone (the same usage counted again) never changes the record
	beat := st(300, at.Add(2*time.Minute))
	c.coalesceUsage("r", beat, at.Add(2*time.Minute))
	if beat.GetUsedAtUnixMs() != at.Add(30*time.Second).UnixMilli() {
		t.Fatalf("heartbeat: %v", beat)
	}
}

func TestReservationDeletedKey(t *testing.T) {
	h := newReservationHandler()
	createRes(t, h, context.Background(), resItem("Event Part", 0, time.Hour, 1000))
	c := monCacheFor(t, h)
	prev := waitObserved(t, h, c, func(s *monState) bool { return len(s.update.GetReservations()) == 1 })
	del, err := h.DeleteReservations(context.Background(), &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{Name: "Event Part"}}})
	wantStates(t, del, err, stDeleted)
	next := waitObserved(t, h, c, func(s *monState) bool { return len(s.update.GetReservations()) == 0 })
	delta, changed := monitoringDelta(prev.update, next.update)
	if !changed || len(delta.GetDeletedKeys()) != 1 {
		t.Fatalf("delta: %v", delta)
	}
	if k := delta.GetDeletedKeys()[0]; k.GetKind() != "Reservation" || k.GetName() != "Event Part" || k.GetLabGroupName() != "" || k.GetNamespace() != "" {
		t.Fatalf("deleted key: %v", k)
	}
	// the filter matches the deleted key with the labels the reservation had
	f, _ := newSelectorFilter("", names.DefaultTenant)
	if got := f.apply(delta, func(key string) map[string]string { return prev.labels[key] }, nil, nil).GetDeletedKeys(); len(got) != 1 {
		t.Fatalf("the owner gets the deletion: %v", got)
	}
}

func TestLabGroupCarriesReservationID(t *testing.T) {
	h := newReservationHandler()
	ctx := context.Background()
	createRes(t, h, ctx, resItem("Event Part 1", -time.Hour, time.Hour, 4000))
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g1", ReservationId: "Event Part 1"}}})
	wantStates(t, res, err, stCreated)
	c := monCacheFor(t, h)
	st := waitObserved(t, h, c, func(s *monState) bool { return len(s.update.GetGroups()) == 1 && len(s.update.GetReservations()) == 1 })
	if got := st.update.GetGroups()[0].GetReservationId(); got != "Event Part 1" {
		t.Fatalf("monitoring gives the reservation id back: %q", got)
	}
	list, err := h.ListLabGroups(ctx, &protobuf.ListRequest{})
	if err != nil || len(list.GetItems()) != 1 || list.GetItems()[0].GetReservationId() != "Event Part 1" {
		t.Fatalf("ListLabGroups: %v %v", list, err)
	}
	if got := reservationIDOf(nil, "plain-name"); got != "plain-name" {
		t.Fatalf("an unknown reservation shows its label: %q", got)
	}
}

func TestEndsAtForExceeded(t *testing.T) {
	r := &laboratoryv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: "r"}}
	r.Spec.To = metav1.NewTime(resNow.Add(time.Hour))
	shrink := metav1.NewTime(resNow.Add(-time.Minute))
	r.Status.Phase = laboratoryv1alpha1.ReservationPhaseActive
	r.Status.Conditions = []metav1.Condition{{Type: laboratoryv1alpha1.ReservationConditionExceeded, Status: metav1.ConditionTrue,
		Reason: laboratoryv1alpha1.ReservationReasonAmountReduced, LastTransitionTime: shrink}}
	st := reservationStatusToProto(r, 5*time.Minute)
	if st.GetPhase() != protobuf.ReservationPhase_RESERVATION_PHASE_ACTIVE || !st.GetExceeded() || st.GetEndingReason() != "AmountReduced" ||
		st.GetEndsAtUnixMs() != shrink.Add(5*time.Minute).UnixMilli() {
		t.Fatalf("exceeded: %v", st)
	}
	// a short notice is at least the operator's minute
	if st = reservationStatusToProto(r, 10*time.Second); st.GetEndsAtUnixMs() != shrink.Add(time.Minute).UnixMilli() {
		t.Fatalf("loss floor: %v", st)
	}
	// served (GroupsDeleted): nothing announced any more
	r.Status.Conditions[0].Status, r.Status.Conditions[0].Reason = metav1.ConditionFalse, laboratoryv1alpha1.ReservationReasonGroupsDeleted
	if st = reservationStatusToProto(r, 5*time.Minute); st.GetExceeded() || st.GetEndsAtUnixMs() != 0 {
		t.Fatalf("served: %v", st)
	}
}
