package laboratory

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

const mi = int64(1) << 20

// under builds a lab of queued pods of 100m and the given memory each, under a reservation, for a tenant.
func under(id, reservation, tenant string, arrival int, mem int64, podNames ...string) *schedObject {
	o := obj("lab/"+id, "", arrival, nil, need(pods(id, podNames...), 100, mem)...)
	o.reservation = reservation
	for _, p := range o.pods {
		p.tenant = tenant
	}
	return o
}

// open is the usage of an active reservation of a tenant.
func open(tenant string, used, amt amount) reservationUsage {
	return reservationUsage{used: used, amount: amt, tenant: tenant, active: true}
}

func reasonOf(t *testing.T, plan schedPlan, id, want string) {
	t.Helper()
	if r := plan.status[id].Reason; r != want {
		t.Fatalf("%s reason = %q (%s), want %q", id, r, plan.status[id].Message, want)
	}
}

// A gang within what is left of its reservation starts; the next gang of the same reservation in the same pass counts what the
// first took and waits with ReservationFull, although the nodes have room.
func TestReservedGangWithinAmount(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, 8*gi)}
	usage := map[string]reservationUsage{"r1": open("t1", amount{cpu: 100, mem: gi}, amount{cpu: 1000, mem: 3 * gi})}
	a := under("a", "r1", "t1", 1, gi, "p1", "p2")
	b := under("b", "r1", "t1", 2, gi, "p1")
	plan := planSchedule([]*schedObject{a, b}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "a/p1", "a/p2")
	reasonOf(t, plan, "lab/b", laboratoryv1alpha1.WaitReservationFull)
	if m := plan.status["lab/b"].Message; m != "the group does not fit what is left of the reservation r1" {
		t.Fatalf("message = %q", m)
	}
	// without the per-node check the amount still holds
	plan = planSchedule([]*schedObject{a, b}, 10, false, &fakeEnv{fair: fairness{usage: usage}})
	wantDispatch(t, plan, "a/p1", "a/p2")
	reasonOf(t, plan, "lab/b", laboratoryv1alpha1.WaitReservationFull)
}

// A gang over what is left of its reservation waits with ReservationFull and does not hold up the gangs behind it (backfill);
// the CPU counts as well as the memory.
func TestReservationFull(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, 8*gi)}
	usage := map[string]reservationUsage{
		"r1": open("t1", amount{cpu: 100, mem: gi + gi/2}, amount{cpu: 1000, mem: 2 * gi}),
		"r2": open("t1", amount{cpu: 950}, amount{cpu: 1000, mem: 2 * gi}),
	}
	a := under("a", "r1", "t1", 1, gi, "p1")
	c := under("c", "r2", "t1", 2, gi, "p1")
	u := under("u", "", "t1", 3, gi, "p1")
	plan := planSchedule([]*schedObject{a, c, u}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "u/p1")
	reasonOf(t, plan, "lab/a", laboratoryv1alpha1.WaitReservationFull)
	reasonOf(t, plan, "lab/c", laboratoryv1alpha1.WaitReservationFull)
	// exactly the amount fits
	usage["r1"] = open("t1", amount{cpu: 100, mem: gi}, amount{cpu: 1000, mem: 2 * gi})
	wantDispatch(t, planSchedule([]*schedObject{a}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}}), "a/p1")
}

// The window decides: before from and from to on, the gangs wait with ReservationNotActive; at from exactly they start.
func TestReservationNotActiveYet(t *testing.T) {
	from := planEpoch.Add(time.Hour)
	res := []laboratoryv1alpha1.Reservation{{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec: laboratoryv1alpha1.ReservationSpec{Tenant: "t1", From: metav1.NewTime(from), To: metav1.NewTime(from.Add(time.Hour)),
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")}},
	}}
	nodes := []nodeRoom{room("n1", 4000, 8*gi)}
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{from.Add(-time.Second), laboratoryv1alpha1.WaitReservationNotActive},
		{from, ""},
		{from.Add(time.Hour - time.Second), ""},
		{from.Add(time.Hour), laboratoryv1alpha1.WaitReservationNotActive},
	} {
		usage := reservationLedger(res, nil, tc.at)
		a := under("a", "r1", "t1", 1, gi, "p1")
		plan := planSchedule([]*schedObject{a}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage, now: tc.at}})
		if tc.want == "" {
			wantDispatch(t, plan, "a/p1")
			continue
		}
		wantDispatch(t, plan)
		reasonOf(t, plan, "lab/a", tc.want)
	}
}

// A reservation of another tenant is as good as none: the gang waits with ReservationMissing, as for a name that does not exist.
func TestReservationOfOtherTenantRefused(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, 8*gi)}
	usage := map[string]reservationUsage{"r2": open("t2", amount{}, amount{cpu: 1000, mem: 2 * gi})}
	a := under("a", "r2", "t1", 1, gi, "p1")
	b := under("b", "nope", "t1", 2, gi, "p1")
	own := under("c", "r2", "t2", 3, gi, "p1")
	plan := planSchedule([]*schedObject{a, b, own}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "c/p1")
	reasonOf(t, plan, "lab/a", laboratoryv1alpha1.WaitReservationMissing)
	reasonOf(t, plan, "lab/b", laboratoryv1alpha1.WaitReservationMissing)
	if m := plan.status["lab/b"].Message; m != "the reservation nope does not exist for the tenant" {
		t.Fatalf("message = %q", m)
	}
	// and the ledger never counts a pod of another tenant against the reservation
	res := []laboratoryv1alpha1.Reservation{{ObjectMeta: metav1.ObjectMeta{Name: "r2"}, Spec: laboratoryv1alpha1.ReservationSpec{Tenant: "t2"}}}
	l := reservationLedger(res, []dispatchedPod{
		{reservation: "r2", tenant: "t1", group: "x", need: amount{cpu: 100, mem: gi}},
		{reservation: "r2", tenant: "t2", group: "y", need: amount{cpu: 200, mem: gi}},
		{reservation: "r2", tenant: "t2", group: "y", need: amount{cpu: 300, mem: gi}},
		{reservation: "r2", tenant: "t2", group: "z", need: amount{cpu: 400, mem: gi}},
	}, planEpoch)
	if u := l["r2"]; u.used != (amount{cpu: 900, mem: 3 * gi}) || u.groups != 2 {
		t.Fatalf("usage = %+v", u)
	}
}

// The unused part of an active reservation is not offered to unreserved work, to a team's network (the lane) without the
// reservation, nor to another reservation short of room; the reservation's own gang gets it. An inactive reservation withholds
// nothing.
func TestUnreservedCannotTakeReservedRoom(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, 4*gi)}
	usage := map[string]reservationUsage{
		"r1": open("t1", amount{}, amount{cpu: 300, mem: 3 * gi}),
		"r2": open("t2", amount{}, amount{cpu: 1000, mem: 2 * gi}),
	}
	fair := fairness{usage: usage}
	// a gang of r1 that waits for its images puts r1 first in the order: r1 keeps 3Gi, r2 the 1Gi left (short of 1Gi)
	w := under("w", "r1", "t1", 0, gi, "p1")
	w.prepKey = "img-w"
	u := under("u", "", "default", 1, 2*gi, "p1")
	other := under("o", "r2", "t2", 2, 2*gi, "p1")
	team := obj("group/team", "", 3, nil, need(pods("group/team", "vpn"), 100, 2*gi)...)
	team.pods[0].tenant = "default"
	env := &fakeEnv{nodes: nodes, empty: nodes, fair: fair, notPrepared: map[string]bool{"img-w": true}}
	plan := planSchedule([]*schedObject{w, u, other, team}, 10, false, env)
	wantDispatch(t, plan)
	reasonOf(t, plan, "lab/w", laboratoryv1alpha1.WaitPreparingImages)
	for _, id := range []string{"lab/u", "lab/o", "group/team"} {
		reasonOf(t, plan, id, laboratoryv1alpha1.WaitInsufficient)
	}
	if w := plan.withheld; w["r1"]["n1"] != (amount{cpu: 300, mem: 3 * gi}) || w["r2"]["n1"] != (amount{cpu: 500, mem: gi}) {
		t.Fatalf("withheld = %+v", w)
	}
	if s := plan.shortfall["r2"]; s != (amount{cpu: 500, mem: gi}) {
		t.Fatalf("shortfall = %+v", plan.shortfall)
	}
	// the reservation's own gang takes it
	mine := under("m", "r1", "t1", 5, 3*gi, "p1")
	plan = planSchedule([]*schedObject{u, mine}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fair})
	wantDispatch(t, plan, "m/p1")
	reasonOf(t, plan, "lab/u", laboratoryv1alpha1.WaitInsufficient)
	// not active: nothing is withheld
	usage["r1"] = reservationUsage{amount: amount{cpu: 300, mem: 3 * gi}, tenant: "t1"}
	usage["r2"] = reservationUsage{amount: amount{cpu: 1000, mem: 2 * gi}, tenant: "t2"}
	small := under("s", "", "default", 4, 2*gi, "p1")
	plan = planSchedule([]*schedObject{u, small}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fair})
	wantDispatch(t, plan, "u/p1", "s/p1")
}

// The withheld room is per node, not a sum: unreserved pods that fit the cluster total less the reservation, but not beside the
// room the reservation keeps on a node, wait. The reservation keeps the emptiest node, with its CPU to memory ratio; its own gang
// uses that room before the free room; what no node holds is its shortfall.
func TestWithheldRoomIsPlacementAware(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, 4*gi), room("n2", 4000, 4*gi)}
	usage := map[string]reservationUsage{"r1": open("t1", amount{}, amount{cpu: 300, mem: 3 * gi})}
	fair := fairness{usage: usage}
	// 8Gi less 3Gi is 5Gi: the sum holds two 2.5Gi pods, the nodes (1Gi and 4Gi offered) only one
	pair := under("u", "", "default", 1, 2*gi+gi/2, "p1", "p2")
	plan := planSchedule([]*schedObject{pair}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fair})
	wantDispatch(t, plan)
	reasonOf(t, plan, "lab/u", laboratoryv1alpha1.WaitInsufficient)
	one := under("v", "", "default", 1, 2*gi+gi/2, "p1")
	plan = planSchedule([]*schedObject{one}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fair})
	wantDispatch(t, plan, "v/p1")
	if !packs([]nodeRoom{room("n1", 3700, gi), room("n2", 4000, 4*gi)}, plan.dispatch) || plan.left[1].Memory != gi+gi/2 {
		t.Fatalf("v must go beside the withheld room: left %+v", plan.left)
	}

	// the emptiest node first: n2 has 4Gi, n1 2Gi; the reservation's 1Gi pod goes onto its own room on n2, so two 2Gi pods
	// of others still fit (n1 2Gi, n2 4Gi less the 1Gi still withheld less the pod)
	nodes = []nodeRoom{room("n1", 4000, 2*gi), room("n2", 4000, 4*gi)}
	usage["r1"] = open("t1", amount{}, amount{cpu: 200, mem: 2 * gi})
	mine := under("m", "r1", "t1", 1, gi, "p1")
	others := under("o", "", "default", 2, 2*gi, "p1", "p2")
	plan = planSchedule([]*schedObject{mine, others}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fair})
	wantDispatch(t, plan, "m/p1", "o/p1", "o/p2")
	if w := plan.withheld["r1"]; len(w) != 1 || w["n2"] != (amount{cpu: 200, mem: 2 * gi}) {
		t.Fatalf("withheld = %+v", w)
	}
	if w := plan.withheldLeft["r1"]; len(w) != 1 || w["n2"] != (amount{cpu: 100, mem: gi}) {
		t.Fatalf("withheld after = %+v", w)
	}

	// spread over nodes with the ratio kept, and the shortfall: 2000m/2Gi on a 1000m/4Gi node keeps 1000m/1Gi
	usage["r1"] = open("t1", amount{}, amount{cpu: 2000, mem: 2 * gi})
	plan = planSchedule(nil, 10, false, &fakeEnv{nodes: []nodeRoom{room("n1", 1000, 4*gi)}, fair: fair})
	if w := plan.withheld["r1"]["n1"]; w != (amount{cpu: 1000, mem: gi}) {
		t.Fatalf("withheld = %+v", plan.withheld)
	}
	if s := plan.shortfall["r1"]; s != (amount{cpu: 1000, mem: gi}) {
		t.Fatalf("shortfall = %+v", plan.shortfall)
	}
	usage["r1"] = open("t1", amount{}, amount{cpu: 3000, mem: 3 * gi})
	plan = planSchedule(nil, 10, false, &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi), room("n2", 4000, 2*gi)}, fair: fair})
	if w := plan.withheld["r1"]; w["n1"] != (amount{cpu: 2000, mem: 2 * gi}) || w["n2"] != (amount{cpu: 1000, mem: gi}) || len(plan.shortfall) != 0 {
		t.Fatalf("withheld = %+v, shortfall %+v", w, plan.shortfall)
	}
}

// A reservation whose resources are zero, negative, incomplete or not cpu and memory reserves nothing: its gangs wait with
// ReservationFull and it withholds no room.
func TestInvalidReservationIsAmountZero(t *testing.T) {
	q := resource.MustParse
	for name, rl := range map[string]corev1.ResourceList{
		"zero memory":  {corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("0")},
		"negative cpu": {corev1.ResourceCPU: q("-1"), corev1.ResourceMemory: q("1Gi")},
		"no cpu":       {corev1.ResourceMemory: q("1Gi")},
		"gpu":          {corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi"), "nvidia.com/gpu": q("1")},
		"empty":        nil,
	} {
		r := &laboratoryv1alpha1.Reservation{Spec: laboratoryv1alpha1.ReservationSpec{Resources: rl}}
		if a := reservationAmount(r); a != (amount{}) {
			t.Fatalf("%s: amount = %+v", name, a)
		}
	}
	valid := &laboratoryv1alpha1.Reservation{Spec: laboratoryv1alpha1.ReservationSpec{
		Resources: corev1.ResourceList{corev1.ResourceCPU: q("1500m"), corev1.ResourceMemory: q("2Gi")}}}
	if a := reservationAmount(valid); a != (amount{cpu: 1500, mem: 2 * gi}) {
		t.Fatalf("amount = %+v", a)
	}
	nodes := []nodeRoom{room("n1", 4000, 4*gi)}
	usage := map[string]reservationUsage{"bad": open("t1", amount{}, amount{})}
	a := under("a", "bad", "t1", 1, gi, "p1")
	zero := under("z", "bad", "t1", 2, 0, "p1")
	zero.pods[0].need = amount{}
	u := under("u", "", "default", 3, 4*gi, "p1")
	plan := planSchedule([]*schedObject{a, zero, u}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "u/p1")
	reasonOf(t, plan, "lab/a", laboratoryv1alpha1.WaitReservationFull)
	reasonOf(t, plan, "lab/z", laboratoryv1alpha1.WaitReservationFull)
	if len(plan.withheld["bad"]) != 0 {
		t.Fatalf("withheld = %+v", plan.withheld)
	}
}

// labUnder puts a lab of the fixture under a reservation of a tenant.
func (f *schedFixture) labUnder(name, reservation, tenantName string) {
	f.t.Helper()
	var l laboratoryv1alpha1.Lab
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + name, Name: name}, &l); err != nil {
		f.t.Fatal(err)
	}
	if l.Labels == nil {
		l.Labels = map[string]string{}
	}
	l.Labels[names.LabelReservation], l.Labels[names.LabelTenant] = reservation, tenantName
	if err := f.c.Update(context.Background(), &l); err != nil {
		f.t.Fatal(err)
	}
}

func (f *schedFixture) addReservation(name, tenantName, cpu, mem string) {
	f.t.Helper()
	f.createPlain(&laboratoryv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-res-" + name)},
		Spec: laboratoryv1alpha1.ReservationSpec{Tenant: tenantName, From: metav1.NewTime(planEpoch), To: metav1.NewTime(planEpoch.Add(24 * time.Hour)),
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}},
	})
}

func (f *schedFixture) reservation(name string) *laboratoryv1alpha1.Reservation {
	f.t.Helper()
	var r laboratoryv1alpha1.Reservation
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: name}, &r); err != nil {
		f.t.Fatal(err)
	}
	return &r
}

// Pods already out count against their reservation and keep running when the reservation shrinks below them: admission only
// holds back what is not dispatched yet (a new lab, and the remainder of an admitted group, which then holds no room).
func TestStartedPodsKeepRunningWhenReservationShrinks(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addReservation("r1", "t1", "1", "1Gi")
	f.addLab("a", "", nil, "p1", "p2")
	f.labUnder("a", "r1", "t1")
	f.tick()
	f.ready("a", "p1")
	f.ready("a", "p2")
	f.tick()
	f.wantStates("a/p1=D a/p2=D")
	r := f.reservation("r1")
	r.Spec.Resources = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("100Mi")}
	if err := f.c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	f.addLab("b", "", nil, "p1")
	f.labUnder("b", "r1", "t1")
	f.tick()
	f.tick()
	f.wantStates("a/p1=D a/p2=D b/p1=Q")
	if st := f.labStatus("b"); st == nil || st.Reason != laboratoryv1alpha1.WaitReservationFull {
		t.Fatalf("b status = %+v", st)
	}
	if u := f.reservation("r1").Status.Used; u.Cpu().MilliValue() != 200 || u.Memory().Value() != 200*mi {
		t.Fatalf("used = %v", u)
	}

	// in the plan: the started pods are never failed, and an admitted remainder over the shrunk amount waits and holds nothing,
	// so an unreserved lab takes the room
	nodes := []nodeRoom{room("n1", 4000, gi)}
	usage := map[string]reservationUsage{"r1": open("t1", amount{cpu: 200, mem: 2 * gi}, amount{cpu: 100, mem: gi})}
	g := under("g", "r1", "t1", 1, gi, "p1:D", "p2")
	g.group = "g1"
	u := under("u", "", "default", 2, gi, "p1")
	plan := planSchedule([]*schedObject{g, u}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "u/p1")
	if len(plan.failed) != 0 {
		t.Fatalf("failed = %+v", plan.failed)
	}
	reasonOf(t, plan, "lab/g", laboratoryv1alpha1.WaitReservationFull)
}

// The Scheduler writes what a reservation uses (the requests of its dispatched pods, of its own tenant only) and how many groups
// run under it, through the rate-limited status writer.
func TestReservationStatusUsedWritten(t *testing.T) {
	cfg := schedCfg()
	cfg.StatusInterval = 10 * time.Second
	f := newSchedFixture(t, cfg)
	f.addReservation("r1", "t1", "2", "2Gi")
	f.addReservation("idle", "t1", "1", "1Gi")
	f.addLab("a", "", nil, "p1", "p2")
	f.labUnder("a", "r1", "t1")
	f.addLab("x", "", nil, "p1")
	f.labUnder("x", "r1", "t2") // another tenant: never counted, waits
	f.tick()
	r := f.reservation("r1")
	if u := r.Status.Used; u.Cpu().MilliValue() != 0 || u.Memory().Value() != 0 || r.Status.Groups != 0 {
		t.Fatalf("before dispatch: used %v groups %d", u, r.Status.Groups)
	}
	f.tick() // within the interval: not written again
	if u := f.reservation("r1").Status.Used; u.Cpu().MilliValue() != 0 {
		t.Fatalf("written within the interval: %v", u)
	}
	f.now = f.now.Add(10 * time.Second)
	f.tick()
	r = f.reservation("r1")
	if u := r.Status.Used; u.Cpu().MilliValue() != 200 || u.Memory().Value() != 200*mi || r.Status.Groups != 1 {
		t.Fatalf("used %v groups %d", u, r.Status.Groups)
	}
	if u := f.reservation("idle").Status.Used; u == nil || u.Cpu().MilliValue() != 0 {
		t.Fatalf("idle used = %v", u)
	}
	if st := f.labStatus("x"); st == nil || st.Reason != laboratoryv1alpha1.WaitReservationMissing {
		t.Fatalf("x status = %+v", st)
	}
	// nothing changed: no write (the stored resourceVersion stays)
	rv := r.ResourceVersion
	f.now = f.now.Add(10 * time.Second)
	f.tick()
	if f.reservation("r1").ResourceVersion != rv {
		t.Fatal("unchanged usage was written again")
	}
}

// The aged gang never keeps the room withheld for another reservation: it packs only beside that room, so a gang that fits
// only on it keeps nothing and the small labs go on; the reservation's own gang still gets its room.
func TestAgedGangNeverKeepsAnotherReservationsRoom(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	usage := map[string]reservationUsage{"r1": open("t1", amount{}, amount{cpu: 200, mem: 2 * gi})}
	a := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, 3*gi)...)
	mine := under("m", "r1", "t1", 0, 2*gi, "p1")
	mine.arrival = agingOn.now.Add(-time.Second)
	fair := agingOn
	fair.usage = usage
	plan := planSchedule([]*schedObject{a, fresh("c", gi), mine}, 10, false, &fakeEnv{nodes: empty, empty: empty, fair: fair})
	wantDispatch(t, plan, "m/p1", "c/p1")
	reasonOf(t, plan, "lab/a", laboratoryv1alpha1.WaitInsufficient)
	// without the reservation the aged gang keeps its room: the small lab waits
	plan = planSchedule([]*schedObject{a, fresh("c", gi)}, 10, false, &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}, empty: empty, fair: agingOn})
	wantDispatch(t, plan)
}

// The remainder of an admitted gang under a reservation counts against it: a new gang of the same reservation that would take it
// over the amount waits with ReservationFull while the remainder dispatches.
func TestHeldRemainderCountsAgainstReservation(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, 8*gi)}
	usage := map[string]reservationUsage{"r1": open("t1", amount{cpu: 100, mem: gi}, amount{cpu: 1000, mem: 3 * gi})}
	g := under("g", "r1", "t1", 1, gi, "p1:D", "p2")
	g.group = "g1"
	b := under("b", "r1", "t1", 2, gi+gi/2, "p1")
	plan := planSchedule([]*schedObject{g, b}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "g/p2")
	reasonOf(t, plan, "lab/b", laboratoryv1alpha1.WaitReservationFull)
}

// A gang of the reservation that does not fit its withheld room alone is placed on the free room and its withheld room together;
// the withheld room then shrinks to what the reservation has left, so the rest is free for the others at once.
func TestOwnGangOutsideItsRoomTrimsTheWithheldRoom(t *testing.T) {
	nodes := []nodeRoom{room("n1", 4000, gi+gi/2), room("n2", 4000, gi+gi/2)}
	usage := map[string]reservationUsage{"r1": open("t1", amount{}, amount{cpu: 200, mem: 2 * gi})}
	// r1 keeps 1.5Gi on n1 and 0.5Gi on n2; its two 1Gi pods do not fit that alone, they go one per node
	mine := under("m", "r1", "t1", 1, gi, "p1", "p2")
	u := under("u", "", "default", 2, gi/2, "p1", "p2")
	plan := planSchedule([]*schedObject{mine, u}, 10, false, &fakeEnv{nodes: nodes, empty: nodes, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "m/p1", "m/p2", "u/p1", "u/p2")
	if w := plan.withheldLeft["r1"]; len(w) != 0 {
		t.Fatalf("withheld after = %+v", w)
	}
}

// An aged gang its reservation does not admit (here full) is not the reserver: the next aged gang keeps its room instead.
func TestAgedGangOverItsReservationIsNotTheReserver(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	nodes := []nodeRoom{room("n1", 4000, gi)}
	full := under("a", "r1", "t1", 0, 3*gi, "p1")
	b := obj("lab/b", "", 1, nil, need(pods("b", "p1"), 100, 3*gi)...)
	fair := agingOn
	// r1 has a quarter Gi left: it withholds that, and c would fit beside it but for the room kept for b
	fair.usage = map[string]reservationUsage{"r1": open("t1", amount{cpu: 100, mem: gi * 7 / 4}, amount{cpu: 1000, mem: 2 * gi})}
	plan := planSchedule([]*schedObject{full, b, fresh("c", gi/2)}, 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: fair})
	wantDispatch(t, plan)
	reasonOf(t, plan, "lab/a", laboratoryv1alpha1.WaitReservationFull)
	reasonOf(t, plan, "lab/c", laboratoryv1alpha1.WaitInsufficient)
}
