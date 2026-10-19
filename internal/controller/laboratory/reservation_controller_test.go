package laboratory

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

const resNotice = 5 * time.Minute

// The window of the test reservations: one hour from resFrom.
var (
	resFrom = planEpoch.Add(time.Hour)
	resTo   = resFrom.Add(time.Hour)
)

type resFixture struct {
	t   *testing.T
	c   client.Client
	r   *ReservationReconciler
	now time.Time
}

func newResFixture(t *testing.T, funcs *interceptor.Funcs) *resFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&laboratoryv1alpha1.Reservation{}, &laboratoryv1alpha1.Tenant{}, &laboratoryv1alpha1.Lab{}, &laboratoryv1alpha1.LabGroup{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	f := &resFixture{t: t, c: b.Build(), now: planEpoch}
	f.r = &ReservationReconciler{Client: f.c, Notice: resNotice, Now: func() time.Time { return f.now }}
	return f
}

func (f *resFixture) tenant(name string) {
	f.t.Helper()
	if err := f.c.Create(context.Background(), &laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *resFixture) dropTenant(name string) {
	f.t.Helper()
	if err := f.c.Delete(context.Background(), &laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		f.t.Fatal(err)
	}
}

// reservation creates a reservation of tenant t1 (cpu 2, memory 2Gi) over the test window.
func (f *resFixture) reservation(name string, res corev1.ResourceList) {
	f.t.Helper()
	if res == nil {
		res = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")}
	}
	if err := f.c.Create(context.Background(), &laboratoryv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       laboratoryv1alpha1.ReservationSpec{Tenant: "t1", From: metav1.NewTime(resFrom), To: metav1.NewTime(resTo), Resources: res},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *resFixture) get(name string) *laboratoryv1alpha1.Reservation {
	f.t.Helper()
	var r laboratoryv1alpha1.Reservation
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: name}, &r); err != nil {
		f.t.Fatal(err)
	}
	return &r
}

func (f *resFixture) reconcile(name string) ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *resFixture) at(d time.Duration, name string) (ctrl.Result, *laboratoryv1alpha1.Reservation) {
	f.t.Helper()
	f.now = resFrom.Add(d)
	res := f.reconcile(name)
	return res, f.get(name)
}

func resLabels(reservation, tenant string) map[string]string {
	l := map[string]string{}
	if reservation != "" {
		l[names.LabelReservation] = reservation
	}
	if tenant != "" {
		l[names.LabelTenant] = tenant
	}
	return l
}

func (f *resFixture) lab(ns, name, reservation, tenant string) {
	f.t.Helper()
	if err := f.c.Create(context.Background(), &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, UID: types.UID("uid-lab-" + name), Labels: resLabels(reservation, tenant)}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *resFixture) group(name, reservation, tenant string) {
	f.t.Helper()
	if err := f.c.Create(context.Background(), &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID("uid-group-" + name), Labels: resLabels(reservation, tenant)}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *resFixture) exists(o client.Object, ns, name string) bool {
	f.t.Helper()
	err := f.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, o)
	if err != nil && !apierrors.IsNotFound(err) {
		f.t.Fatal(err)
	}
	return err == nil
}

func (f *resFixture) labExists(ns, name string) bool {
	return f.exists(&laboratoryv1alpha1.Lab{}, ns, name)
}

func (f *resFixture) groupExists(name string) bool {
	return f.exists(&laboratoryv1alpha1.LabGroup{}, "", name)
}

func endingCond(r *laboratoryv1alpha1.Reservation) *metav1.Condition {
	return meta.FindStatusCondition(r.Status.Conditions, laboratoryv1alpha1.ReservationConditionEnding)
}

func wantPhase(t *testing.T, r *laboratoryv1alpha1.Reservation, phase laboratoryv1alpha1.ReservationPhase) {
	t.Helper()
	if r.Status.Phase != phase {
		t.Fatalf("phase = %q, want %q (status %+v)", r.Status.Phase, phase, r.Status)
	}
}

func wantAfter(t *testing.T, res ctrl.Result, d time.Duration) {
	t.Helper()
	if res.RequeueAfter != d {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, d)
	}
}

// The phase follows the clock: Pending before from, Active, Ending from to - notice (noticeAt and the Ending condition set
// once), Ended at to. Every reconcile asks to come back at the next milestone, at most a minute later while the reservation
// has not ended (its tenant may go), and at the removal a day after the end once it has.
func TestPhaseMilestones(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)

	res, r := f.at(-30*time.Second, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhasePending)
	wantAfter(t, res, 30*time.Second)
	if endingCond(r) != nil || r.Status.NoticeAt != nil {
		t.Fatalf("pending has ending state: %+v", r.Status)
	}
	res, r = f.at(-10*time.Minute, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhasePending)
	wantAfter(t, res, reservationRecheck)

	res, r = f.at(0, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseActive)
	wantAfter(t, res, reservationRecheck)
	res, r = f.at(time.Hour-resNotice-20*time.Second, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseActive)
	wantAfter(t, res, 20*time.Second)

	noticeAt := resTo.Add(-resNotice)
	res, r = f.at(time.Hour-resNotice, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnding)
	wantAfter(t, res, reservationRecheck)
	if r.Status.NoticeAt == nil || !r.Status.NoticeAt.Time.Equal(noticeAt) {
		t.Fatalf("noticeAt = %v, want %v", r.Status.NoticeAt, noticeAt)
	}
	c := endingCond(r)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != laboratoryv1alpha1.ReservationReasonWindowClosing {
		t.Fatalf("ending condition = %+v", c)
	}
	res, r = f.at(time.Hour-10*time.Second, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnding)
	wantAfter(t, res, 10*time.Second)
	if !r.Status.NoticeAt.Time.Equal(noticeAt) {
		t.Fatalf("the notice time moved: %v", r.Status.NoticeAt)
	}

	res, r = f.at(time.Hour, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnded)
	wantAfter(t, res, reservationRetention)
	if c := endingCond(r); c == nil || c.Status != metav1.ConditionFalse || c.Reason != laboratoryv1alpha1.ReservationReasonEnded {
		t.Fatalf("ending condition after the end = %+v", c)
	}
	if !r.Status.NoticeAt.Time.Equal(noticeAt) {
		t.Fatalf("noticeAt lost after the end: %v", r.Status.NoticeAt)
	}
	rv := r.ResourceVersion
	if _, r = f.at(time.Hour+time.Minute, "r"); r.ResourceVersion != rv {
		t.Fatal("an unchanged status was written again")
	}

	// A window shorter than the notice: the notice comes before from, so Pending waits for it, not for from.
	short := newResFixture(t, nil)
	short.tenant("t1")
	short.reservation("s", nil)
	short.r.Notice = 2 * time.Hour
	short.now = resFrom.Add(-time.Hour - 20*time.Second)
	wantAfter(t, short.reconcile("s"), 20*time.Second)
	short.now = resFrom.Add(-time.Hour)
	short.reconcile("s")
	wantPhase(t, short.get("s"), laboratoryv1alpha1.ReservationPhaseEnding)
}

// At the end every LabGroup and Lab of the reservation goes in one pass: groups and labs in group namespaces, labs of their
// own. Everything else stays.
func TestEndDeletesAllGroupsOfReservation(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)
	for _, g := range []string{"g1", "g2"} {
		f.group(g, "r", "t1")
		f.lab(laboratoryv1alpha1.LabGroupNamespace(g), g+"-a", "r", "t1")
		f.lab(laboratoryv1alpha1.LabGroupNamespace(g), g+"-b", "r", "t1")
	}
	f.lab("ns-solo", "solo", "r", "t1")
	f.group("free", "", "t1")
	f.lab("ns-free", "free", "", "t1")

	f.at(time.Hour-time.Second, "r")
	if !f.groupExists("g1") || !f.labExists("ns-solo", "solo") {
		t.Fatal("deleted before the end")
	}
	f.at(time.Hour, "r")
	for _, g := range []string{"g1", "g2"} {
		if f.groupExists(g) || f.labExists(laboratoryv1alpha1.LabGroupNamespace(g), g+"-a") || f.labExists(laboratoryv1alpha1.LabGroupNamespace(g), g+"-b") {
			t.Fatalf("group %s or its labs survived the end", g)
		}
	}
	if f.labExists("ns-solo", "solo") {
		t.Fatal("the lab of its own survived the end")
	}
	if !f.groupExists("free") || !f.labExists("ns-free", "free") {
		t.Fatal("unreserved objects were deleted")
	}
}

// Every delete names the UID it listed: an object that was replaced under the same name is not deleted blindly, and the pass
// is repeated soon.
func TestEndDeletesWithUIDPreconditions(t *testing.T) {
	var uids []types.UID
	f := newResFixture(t, &interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
		do := &client.DeleteOptions{}
		do.ApplyOptions(opts)
		if do.Preconditions == nil || do.Preconditions.UID == nil {
			t.Errorf("delete of %s without a UID precondition", o.GetName())
		} else {
			uids = append(uids, *do.Preconditions.UID)
		}
		if o.GetName() == "replaced" {
			return apierrors.NewConflict(schema.GroupResource{Group: "laboratory.cybericebox.com", Resource: "labs"}, o.GetName(), nil)
		}
		return c.Delete(ctx, o, opts...)
	}})
	f.tenant("t1")
	f.reservation("r", nil)
	f.group("g", "r", "t1")
	f.lab("ns-replaced", "replaced", "r", "t1")
	res, _ := f.at(time.Hour, "r")
	if len(uids) != 2 || uids[0] != "uid-group-g" || uids[1] != "uid-lab-replaced" {
		t.Fatalf("preconditions = %v", uids)
	}
	wantAfter(t, res, reservationRecheck)
}

// An extension after the notice calls the end off: the Ending condition goes False, noticeAt goes, and at the old end nothing
// is deleted.
func TestExtendAfterNoticeCancelsDeletion(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)
	f.group("g", "r", "t1")
	f.lab("ns-solo", "solo", "r", "t1")

	_, r := f.at(time.Hour-time.Minute, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnding)
	r.Spec.To = metav1.NewTime(resTo.Add(time.Hour))
	if err := f.c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	_, r = f.at(time.Hour-30*time.Second, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseActive)
	if r.Status.NoticeAt != nil {
		t.Fatalf("noticeAt kept after the extension: %v", r.Status.NoticeAt)
	}
	if c := endingCond(r); c == nil || c.Status != metav1.ConditionFalse || c.Reason != laboratoryv1alpha1.ReservationReasonExtended {
		t.Fatalf("ending condition after the extension = %+v", c)
	}
	_, r = f.at(time.Hour, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseActive)
	if !f.groupExists("g") || !f.labExists("ns-solo", "solo") {
		t.Fatal("groups deleted at the old end")
	}
	// the new notice is given at the new time
	_, r = f.at(2*time.Hour-resNotice, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnding)
	if !r.Status.NoticeAt.Time.Equal(resTo.Add(time.Hour - resNotice)) {
		t.Fatalf("new noticeAt = %v", r.Status.NoticeAt)
	}
}

// An operator that was down over the end processes it on its first reconcile: the groups go at once, and the notice that was
// never given is not made up.
func TestPastEndOnStartDeletesGroups(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)
	f.at(30*time.Minute, "r") // last seen Active
	f.group("g", "r", "t1")
	f.lab(laboratoryv1alpha1.LabGroupNamespace("g"), "g-a", "r", "t1")
	f.lab("ns-solo", "solo", "r", "t1")

	restarted := &ReservationReconciler{Client: f.c, Notice: resNotice, Now: func() time.Time { return resTo.Add(10 * time.Minute) }}
	res, err := restarted.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	r := f.get("r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnded)
	if r.Status.NoticeAt != nil {
		t.Fatalf("noticeAt made up: %v", r.Status.NoticeAt)
	}
	if f.groupExists("g") || f.labExists(laboratoryv1alpha1.LabGroupNamespace("g"), "g-a") || f.labExists("ns-solo", "solo") {
		t.Fatal("a missed end did not delete the groups")
	}
	wantAfter(t, res, reservationRetention-10*time.Minute)
}

// An ended reservation stays a day after its end (the backend and the admin see how it ended), then it is deleted.
func TestEndedRemovedAfter24h(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)
	f.at(time.Hour, "r")
	res, r := f.at(time.Hour+reservationRetention-time.Second, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnded)
	wantAfter(t, res, time.Second)
	f.now = resTo.Add(reservationRetention)
	if res := f.reconcile("r"); res.RequeueAfter != 0 {
		t.Fatalf("requeue after the removal: %v", res)
	}
	if f.exists(&laboratoryv1alpha1.Reservation{}, "", "r") {
		t.Fatal("the reservation is still there a day after its end")
	}
	f.reconcile("r") // gone: nothing to do
}

// Resources that reserve nothing (zero, negative, missing, or a name other than cpu and memory) give Invalid=True with the
// reason; the phase still follows the clock and the scheduler counts the amount as 0. A fix removes the condition.
func TestInvalidReservationCondition(t *testing.T) {
	q := resource.MustParse
	for name, res := range map[string]corev1.ResourceList{
		"zero-cpu":   {corev1.ResourceCPU: q("0"), corev1.ResourceMemory: q("1Gi")},
		"neg-memory": {corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("-1Gi")},
		"no-memory":  {corev1.ResourceCPU: q("1")},
		"gpu":        {corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi"), "nvidia.com/gpu": q("1")},
		"empty":      {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newResFixture(t, nil)
			f.tenant("t1")
			f.reservation(name, res)
			_, r := f.at(time.Minute, name)
			wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseActive)
			c := meta.FindStatusCondition(r.Status.Conditions, laboratoryv1alpha1.ReservationConditionInvalid)
			if c == nil || c.Status != metav1.ConditionTrue || c.Message == "" {
				t.Fatalf("invalid condition = %+v", c)
			}
			if a := reservationAmount(r); a != (amount{}) {
				t.Fatalf("an invalid reservation reserves %+v", a)
			}
			r.Spec.Resources = corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi")}
			if err := f.c.Update(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			_, r = f.at(2*time.Minute, name)
			if c := meta.FindStatusCondition(r.Status.Conditions, laboratoryv1alpha1.ReservationConditionInvalid); c != nil {
				t.Fatalf("the condition stays after the fix: %+v", c)
			}
		})
	}
}

// The end deletes only what runs under THIS reservation: labs of another reservation, unlabelled ones and another tenant's
// objects with the label stay. A group of the reservation that still holds labs of others is kept (its deletion would drain
// them) and deleted on a later pass once they are gone.
func TestEndKeepsOtherReservationsObjects(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)
	f.reservation("r2", nil)
	mixed := laboratoryv1alpha1.LabGroupNamespace("mixed")
	f.group("mixed", "r", "t1")
	f.lab(mixed, "mine", "r", "t1")
	f.lab(mixed, "theirs", "r2", "t1")
	f.group("other", "r2", "t1")
	f.lab("ns-loose", "loose", "", "t1")
	f.lab("ns-alien", "alien", "r", "t2")
	f.group("alien", "r", "t2")

	res, _ := f.at(time.Hour, "r")
	if f.labExists(mixed, "mine") {
		t.Fatal("the reservation's own lab in the mixed group survived")
	}
	for _, keep := range [][2]string{{mixed, "theirs"}, {"ns-loose", "loose"}, {"ns-alien", "alien"}} {
		if !f.labExists(keep[0], keep[1]) {
			t.Fatalf("lab %s/%s was deleted", keep[0], keep[1])
		}
	}
	for _, g := range []string{"mixed", "other", "alien"} {
		if !f.groupExists(g) {
			t.Fatalf("group %s was deleted", g)
		}
	}
	wantAfter(t, res, reservationRecheck)

	if err := f.c.Delete(context.Background(), &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Namespace: mixed, Name: "theirs"}}); err != nil {
		t.Fatal(err)
	}
	res, _ = f.at(time.Hour+time.Minute, "r")
	if f.groupExists("mixed") {
		t.Fatal("the group was kept after the others' labs went")
	}
	if !f.groupExists("other") || !f.groupExists("alien") {
		t.Fatal("another reservation's or tenant's group was deleted")
	}
	wantAfter(t, res, reservationRetention-time.Minute)
}

// A reservation whose tenant is gone ends: first the notice (Ending, reason TenantGone, from the moment it is seen), then,
// one notice later, its groups go, long before spec.to. It stays ended even if a tenant of that name comes back, and the
// scheduler no longer counts it open. A tenant back during the notice calls the end off.
func TestTenantGoneEndsReservation(t *testing.T) {
	f := newResFixture(t, nil)
	f.tenant("t1")
	f.reservation("r", nil)
	f.group("g", "r", "t1")
	f.at(10*time.Minute, "r")

	f.dropTenant("t1")
	seen := resFrom.Add(20 * time.Minute)
	res, r := f.at(20*time.Minute, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnding)
	wantAfter(t, res, resNotice)
	if c := endingCond(r); c == nil || c.Status != metav1.ConditionTrue || c.Reason != laboratoryv1alpha1.ReservationReasonTenantGone {
		t.Fatalf("ending condition = %+v", c)
	}
	if !r.Status.NoticeAt.Time.Equal(seen) {
		t.Fatalf("noticeAt = %v, want %v", r.Status.NoticeAt, seen)
	}
	if !f.groupExists("g") {
		t.Fatal("deleted without the notice")
	}
	res, _ = f.at(20*time.Minute+resNotice-time.Second, "r")
	wantAfter(t, res, time.Second)
	if !f.groupExists("g") {
		t.Fatal("deleted before the notice ran out")
	}

	_, r = f.at(20*time.Minute+resNotice, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnded)
	if f.groupExists("g") {
		t.Fatal("the group of a gone tenant survived the notice")
	}
	if reservationActive(r, f.now) {
		t.Fatal("the scheduler still counts the ended reservation open")
	}

	f.tenant("t1")
	res, r = f.at(30*time.Minute, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnded)
	wantAfter(t, res, seen.Add(resNotice+reservationRetention).Sub(f.now))

	// back during the notice: the end is called off
	g := newResFixture(t, nil)
	g.tenant("t1")
	g.reservation("r", nil)
	g.group("g", "r", "t1")
	g.dropTenant("t1")
	_, r = g.at(10*time.Minute, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseEnding)
	g.tenant("t1")
	_, r = g.at(12*time.Minute, "r")
	wantPhase(t, r, laboratoryv1alpha1.ReservationPhaseActive)
	if c := endingCond(r); c == nil || c.Status != metav1.ConditionFalse || r.Status.NoticeAt != nil {
		t.Fatalf("the end of a returned tenant was not called off: %+v", r.Status)
	}
	g.at(10*time.Minute+resNotice, "r")
	if !g.groupExists("g") {
		t.Fatal("the group of a returned tenant was deleted")
	}
}

// The scheduler owns status.used and status.groups, the reconciler phase, noticeAt and the conditions; each writes a merge
// patch of its own fields, so neither erases the other's: in either order, from a stale copy, and when the scheduler writes
// between the reconciler's read and its patch (the reconciler's patch then conflicts and the retry keeps both).
func TestSchedulerAndReconcilerStatusFieldsCoexist(t *testing.T) {
	sf := newSchedFixture(t, schedCfg())
	sf.s.lastWrite = map[types.UID]time.Time{}
	ctx := context.Background()
	if err := sf.c.Create(ctx, &laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "t1"}}); err != nil {
		t.Fatal(err)
	}
	sf.addReservation("r", "t1", "2", "2Gi")
	rec := &ReservationReconciler{Client: sf.c, Notice: resNotice, Now: func() time.Time { return sf.now }}
	reconcile := func() {
		t.Helper()
		if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "r"}}); err != nil {
			t.Fatal(err)
		}
	}
	schedWrite := func(stale *laboratoryv1alpha1.Reservation, cpu int64) {
		t.Helper()
		sf.now = sf.now.Add(time.Second)
		sf.s.writeReservations(ctx, []laboratoryv1alpha1.Reservation{*stale}, map[string]reservationUsage{"r": {used: amount{cpu: cpu, mem: 64 * mi}, groups: 1}}, sf.now, time.Nanosecond, 10, map[types.UID]bool{})
	}
	check := func(phase laboratoryv1alpha1.ReservationPhase, cpu int64) {
		t.Helper()
		r := sf.reservation("r")
		if r.Status.Phase != phase || r.Status.Used.Cpu().MilliValue() != cpu || r.Status.Groups != 1 {
			t.Fatalf("status = %+v, want phase %s and used cpu %dm", r.Status, phase, cpu)
		}
	}

	// scheduler first, then the reconciler
	schedWrite(sf.reservation("r"), 100)
	reconcile()
	check(laboratoryv1alpha1.ReservationPhaseActive, 100)

	// the scheduler writes from a copy it read before the reconciler gave the notice
	stale := sf.reservation("r")
	sf.now = planEpoch.Add(24*time.Hour - resNotice)
	reconcile()
	schedWrite(stale, 200)
	check(laboratoryv1alpha1.ReservationPhaseEnding, 200)
	r := sf.reservation("r")
	if r.Status.NoticeAt == nil || endingCond(r) == nil || endingCond(r).Status != metav1.ConditionTrue {
		t.Fatalf("the scheduler's write erased the notice: %+v", r.Status)
	}

	// the scheduler writes between the reconciler's read and its patch
	sf.now = planEpoch.Add(24 * time.Hour)
	wrote := false
	rec.Client = interceptor.NewClient(sf.c.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			if !wrote {
				wrote = true
				schedWrite(sf.reservation("r"), 300)
			}
			return c.SubResource(sub).Patch(ctx, o, p, opts...)
		},
	})
	_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "r"}})
	if !apierrors.IsConflict(err) {
		t.Fatalf("a patch over a newer status must conflict, got %v", err)
	}
	check(laboratoryv1alpha1.ReservationPhaseEnding, 300)
	reconcile()
	check(laboratoryv1alpha1.ReservationPhaseEnded, 300)
	if c := endingCond(sf.reservation("r")); c == nil || c.Reason != laboratoryv1alpha1.ReservationReasonEnded {
		t.Fatalf("ending condition = %+v", c)
	}
}
