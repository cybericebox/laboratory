package laboratory

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// orphaned starts the lab r under r1 of t1 and the unreserved lab u, queues the lab w under r1, then deletes r1 by hand.
func orphaned(f *schedFixture) {
	f.t.Helper()
	f.addReservation("r1", "t1", "500m", "500Mi")
	f.addLab("r", "", nil, "p1")
	f.labUnder("r", "r1", "t1")
	f.addLab("u", "", nil, "p1")
	f.tick()
	f.wantStates("r/p1=S u/p1=S")
	f.running("r", "p1")
	if err := f.c.Delete(context.Background(), f.reservation("r1")); err != nil {
		f.t.Fatal(err)
	}
	f.addLab("w", "", nil, "p1")
	f.labUnder("w", "r1", "t1")
}

// A Reservation deleted by hand leaves no work behind: what ran under it gets the notice of an ended reservation and is then
// deleted; a lab under the gone name that never ran only waits (ReservationMissing), and unreserved work stays.
func TestGoneReservationWorkDeletedAfterNotice(t *testing.T) {
	f := shortFixture(t)
	orphaned(f)
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("deleted at once")
	}
	if st := f.labStatus("w"); st == nil || st.Reason != laboratoryv1alpha1.WaitReservationMissing {
		t.Fatalf("w status = %+v", st)
	}
	f.now = f.now.Add(5*time.Minute - 2*time.Second)
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("deleted before the notice ran out")
	}
	f.tick()
	if f.lab("r") != nil {
		t.Fatal("the work of the gone reservation survived its notice")
	}
	if f.lab("w") == nil || f.lab("u") == nil {
		t.Fatal("deleted a lab that never ran under it, or unreserved work")
	}
}

// The reservation is checked past the cache before anything goes: a cache that misses it (here: the server has it) deletes
// nothing, and a reservation that comes back before the end of the notice calls it off.
func TestGoneReservationCheckedUncached(t *testing.T) {
	f := shortFixture(t)
	orphaned(f)
	f.tick()
	there := true
	f.s.Reader = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if r, ok := obj.(*laboratoryv1alpha1.Reservation); ok && there {
				r.Name = key.Name
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("deleted on the cache alone")
	}
	there = false
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("tried again before the retry time")
	}
	f.now = f.now.Add(evictRetryAfter)
	f.tick()
	if f.lab("r") != nil {
		t.Fatal("not deleted once the server agrees")
	}

	// back before the notice ran out: nothing goes, and a second loss gets a new full notice
	g := shortFixture(t)
	orphaned(g)
	g.tick()
	g.now = g.now.Add(3 * time.Minute)
	g.addReservation("r1", "t1", "500m", "500Mi")
	g.tick()
	if err := g.c.Delete(context.Background(), g.reservation("r1")); err != nil {
		t.Fatal(err)
	}
	g.now = g.now.Add(3 * time.Minute)
	g.tick()
	if g.lab("r") == nil {
		t.Fatal("deleted on the notice of the first loss")
	}
}

// A gap in the passes (a restart, or no pass for longer than the record lives) starts the notice again.
func TestGoneReservationFreshNoticeAfterGap(t *testing.T) {
	f := shortFixture(t)
	orphaned(f)
	f.tick()
	f.now = f.now.Add(4 * time.Minute)
	f.s = &Scheduler{Client: f.c, Config: f.s.Config, Defaults: f.s.Defaults, Now: f.s.Now}
	f.tick()
	f.now = f.now.Add(2 * time.Minute)
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("deleted on a notice from before the restart")
	}
	f.now = f.now.Add(3 * time.Minute)
	f.tick()
	if f.lab("r") != nil {
		t.Fatal("not deleted after the new notice")
	}
}

// A LabGroup that ran under the gone reservation goes with its labs; one whose namespace also holds other work stays.
func TestGoneReservationGroups(t *testing.T) {
	f := shortFixture(t)
	ctx := context.Background()
	orphaned(f)
	group := func(name, ns string) {
		g := &laboratoryv1alpha1.LabGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), CreationTimestamp: metav1.NewTime(planEpoch),
				Labels: map[string]string{names.LabelReservation: "r1", names.LabelTenant: "t1"}},
			Status: laboratoryv1alpha1.LabGroupStatus{Namespace: ns, Pods: []laboratoryv1alpha1.NamedPodSchedule{
				{Name: names.ComponentGateway, PodSchedule: laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodStarted}},
			}},
		}
		g.Spec.VPN.Disabled = true
		f.create(g)
	}
	group("own", "ns-r")   // holds r only
	group("mixed", "ns-u") // holds the unreserved u
	exists := func(name string) bool {
		var g laboratoryv1alpha1.LabGroup
		err := f.c.Get(ctx, types.NamespacedName{Name: name}, &g)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	f.tick()
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	if exists("own") || f.lab("r") != nil {
		t.Fatal("the group of the gone reservation survived")
	}
	if !exists("mixed") || f.lab("u") == nil {
		t.Fatal("a group that holds other work was deleted")
	}
}

// Without the Reservation CRD nothing is taken for the work of a gone reservation: the list says nothing about what exists.
func TestNoOrphansWithoutTheReservationKind(t *testing.T) {
	f := shortFixture(t)
	orphaned(f)
	snap := mustLoad(t, f)
	f.s.noReservations = true
	if got := f.s.orphanTargets(snap); len(got) != 0 {
		t.Fatalf("targets without the kind: %v", got)
	}
	f.s.noReservations = false
	if got := f.s.orphanTargets(snap); len(got) != 1 || got["lab/ns-r/r"] != "r1" {
		t.Fatalf("targets = %v", got)
	}
}

func mustLoad(t *testing.T, f *schedFixture) *clusterView {
	t.Helper()
	snap, err := f.s.load(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// The eviction record keeps the time of its pass: a pass more than seenMaxAge after the last one gives a new notice even in
// the same process, and Start forgets what an earlier run saw.
func TestEvictRecordDroppedAfterAGap(t *testing.T) {
	f := shortFixture(t)
	f.s.Config.Tick = 0 // the default tick: the record lives a minute
	unreservedHolding(f)
	f.tick()
	f.now = f.now.Add(5*time.Minute + 30*time.Second)
	f.tick()
	if f.lab("u") == nil {
		t.Fatal("deleted after a gap of five minutes between two passes")
	}
	if got, want := f.evictAtOf("u"), f.now.Add(5*time.Minute).UTC().Format(time.RFC3339); got != want {
		t.Fatalf("evict-at = %q, want the new notice %q", got, want)
	}
	for range 11 {
		f.now = f.now.Add(29 * time.Second)
		f.tick()
	}
	if f.lab("u") != nil {
		t.Fatal("not deleted after the new notice, seen on every pass")
	}

	f.s.evictSeen = passRecord{at: f.now, first: map[types.UID]time.Time{"x": f.now}}
	f.s.orphanSeen = f.s.evictSeen
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if f.s.evictSeen.first != nil || f.s.orphanSeen.first != nil {
		t.Fatal("Start kept the records of an earlier run")
	}
}

// With what stopped labs hold unknown the capacity is unknown for the whole pass: the nodes are listed once, not on every
// question of the plan.
func TestUnknownCapacityAskedOncePerPass(t *testing.T) {
	f := shortFixture(t)
	f.addLab("a", "", nil, "p1")
	f.addLab("b", "", nil, "p1")
	f.addLab("s", "", nil, "p1")
	stopped := f.lab("s")
	stopped.Spec.Lifecycle = &laboratoryv1alpha1.LabLifecycleSpec{DesiredState: "Stopped"}
	if err := f.c.Update(context.Background(), stopped); err != nil {
		t.Fatal(err)
	}
	// a stopped lab whose device ran but whose node and pod are unknown: what it holds is unknown
	d := f.device("s", "p1")
	d.Status.Scheduling.State = laboratoryv1alpha1.PodStarted
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	snap := mustLoad(t, f)
	if _, _, unknown := f.s.stoppedReservations(snap); !unknown {
		t.Fatal("the fixture does not make the stopped hold unknown")
	}
	lists := 0
	f.s.Client = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.NodeList); ok {
				lists++
			}
			return c.List(ctx, list, opts...)
		},
	})
	f.tick()
	if lists != 1 {
		t.Fatalf("the nodes were listed %d times in one pass", lists)
	}
	f.wantStates("a/p1=Q b/p1=Q s/p1=D")
}

// bareLab creates a Lab of the deploy group with one container device in its spec and no Device (its reconciler made none),
// created now.
func (f *schedFixture) bareLab(name, group string, phase laboratoryv1alpha1.Phase) {
	f.t.Helper()
	f.create(&laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-" + name, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(f.now), Labels: map[string]string{names.LabelDeployGroup: group}},
		Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
			{Name: "p1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "reg/" + name}}},
		Status: laboratoryv1alpha1.LabStatus{Phase: phase},
	})
}

// A failed lab (no Devices: its graph did not validate) is no member of its gang: the gang starts at once.
func TestFailedLabNeverBlocksItsGang(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g", nil, "p1")
	f.bareLab("b", "g", laboratoryv1alpha1.PhaseFailed)
	f.tick()
	f.wantStates("a/p1=S")
}

// A lab whose Devices never come holds its gang only within the settle window, then the gang goes on and the lab gets one
// Warning event.
func TestStuckLabHoldsItsGangOnlyForTheWindow(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	rec := record.NewFakeRecorder(10)
	f.s.Recorder = rec
	f.addLab("a", "g", nil, "p1")
	f.bareLab("b", "g", "")
	f.tick()
	f.wantStates("a/p1=Q")
	if st := f.labStatus("a"); st == nil || st.Message != "waiting for the pods of lab/ns-b/b to be set up" {
		t.Fatalf("a status = %+v", st)
	}
	f.now = f.now.Add(unsetSettleWindow)
	f.tick()
	f.wantStates("a/p1=S")
	f.tick()
	if n := len(rec.Events); n != 1 {
		t.Fatalf("%d events, want one warning", n)
	}
	if e := <-rec.Events; !strings.Contains(e, "PodsNotSetUp") {
		t.Fatalf("event = %q", e)
	}
}

// A deleted Reservation CRD takes every Reservation with it and makes their Get a NotFound: that is no reservation gone.
// Before any deletion the scheduler asks discovery: the kind not served deletes nothing and drops every notice.
func TestDeletedReservationCRDDeletesNothing(t *testing.T) {
	f := shortFixture(t)
	served := false
	f.s.ReservationKindServed = func(context.Context) (bool, error) { return served, nil }
	orphaned(f)
	f.tick()
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("deleted while the Reservation kind is not served")
	}
	if f.s.orphanSeen.first != nil {
		t.Fatal("the notices were kept")
	}
	served = true
	f.tick()
	if f.lab("r") == nil {
		t.Fatal("deleted at once when the kind came back")
	}
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	if f.lab("r") != nil {
		t.Fatal("not deleted one notice after the kind came back")
	}
}

// The labs of an orphaned LabGroup's namespace are read past the cache before it goes: a lab of another tenant that only the
// server knows keeps the group.
func TestOrphanedGroupChecksItsLabsUncached(t *testing.T) {
	f := shortFixture(t)
	ctx := context.Background()
	orphaned(f)
	g := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "own", UID: "uid-own", CreationTimestamp: metav1.NewTime(planEpoch),
			Labels: map[string]string{names.LabelReservation: "r1", names.LabelTenant: "t1"}},
		Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "ns-r", Pods: []laboratoryv1alpha1.NamedPodSchedule{
			{Name: names.ComponentGateway, PodSchedule: laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodStarted}},
		}},
	}
	g.Spec.VPN.Disabled = true
	f.create(g)
	f.s.Reader = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			lo := (&client.ListOptions{}).ApplyOptions(opts)
			if ll, ok := list.(*laboratoryv1alpha1.LabList); ok && lo.Namespace == "ns-r" {
				ll.Items = append(ll.Items, laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "ns-r",
					Labels: map[string]string{names.LabelTenant: "t2"}}})
			}
			return nil
		},
	})
	f.tick()
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	var got laboratoryv1alpha1.LabGroup
	if err := f.c.Get(ctx, types.NamespacedName{Name: "own"}, &got); err != nil {
		t.Fatalf("the group that holds a lab only the server knows was deleted: %v", err)
	}
}

// An operator that started without the Reservation controller admits nothing under a reservation until it is restarted,
// even once the CRD is there: no window would end.
func TestNoReservationControllerAdmitsNoReservedWork(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.s.NoReservationController = true
	f.addReservation("r1", "t1", "1", "1Gi")
	f.addLab("r", "", nil, "p1")
	f.labUnder("r", "r1", "t1")
	f.addLab("u", "", nil, "p1")
	f.tick()
	f.wantStates("r/p1=Q u/p1=S")
	if st := f.labStatus("r"); st == nil || st.Reason != laboratoryv1alpha1.WaitReservationMissing {
		t.Fatalf("r status = %+v", st)
	}
}
