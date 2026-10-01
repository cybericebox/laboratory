package laboratory

import (
	"strings"
	"testing"
	"time"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var planEpoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const (
	qd = laboratoryv1alpha1.PodQueued
	st = laboratoryv1alpha1.PodStarting
	sd = laboratoryv1alpha1.PodStarted
	fl = laboratoryv1alpha1.PodFailed
)

// pods builds the pods of an object from "name:state" pairs; a bare name is Queued
// and "name:" is a pod nobody initialised yet.
func pods(obj string, specs ...string) []*schedPod {
	var out []*schedPod
	for _, sp := range specs {
		name, state, _ := strings.Cut(sp, ":")
		p := &schedPod{key: obj + "/" + name, name: name, kind: kindDevicePod, state: qd}
		switch state {
		case "":
			if strings.Contains(sp, ":") {
				p.state = ""
			}
		case "S":
			p.state = st
		case "D":
			p.state = sd
		case "F":
			p.state = fl
		}
		out = append(out, p)
	}
	return out
}

func obj(id, group string, arrival int, after []string, ps ...*schedPod) *schedObject {
	return &schedObject{id: id, group: group, after: after, arrival: planEpoch.Add(time.Duration(arrival) * time.Second), pods: ps}
}

type fakeEnv struct {
	notPrepared map[string]bool // by prepKey
	fits        map[string]fit  // by pod key
	taken       int
}

func (f *fakeEnv) prepared(o *schedObject) bool { return !f.notPrepared[o.prepKey] }
func (f *fakeEnv) check(p *schedPod) fit {
	if v, ok := f.fits[p.key]; ok {
		return v
	}
	return fitOK
}
func (f *fakeEnv) take(*schedPod) { f.taken++ }

func keys(ps []*schedPod) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.key)
	}
	return out
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wantDispatch(t *testing.T, plan schedPlan, want ...string) {
	t.Helper()
	if got := keys(plan.dispatch); !same(got, want) {
		t.Fatalf("dispatch = %v, want %v", got, want)
	}
}

// An object may start with fewer slots than pods: 3 slots, 5 pods, 3 now.
func TestPlanPartialStart(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, pods("a", "p1", "p2", "p3", "p4", "p5")...)
	plan := planSchedule([]*schedObject{o}, 3, false, &fakeEnv{})
	wantDispatch(t, plan, "a/p1", "a/p2", "a/p3")
	st := plan.status["lab/a"]
	if st.Position != 1 || st.Length != 1 || st.Pending != 2 || st.Pods != 5 || st.Reason != laboratoryv1alpha1.WaitInFlightLimit {
		t.Fatalf("status = %+v", st)
	}
}

// Once an object started, no other object's pods interleave with its rest.
func TestPlanConveyorDoesNotInterleave(t *testing.T) {
	o1 := obj("lab/a", "g", 1, nil, pods("a", "p1:S", "p2:S", "p3")...)
	o2 := obj("lab/b", "g", 2, nil, pods("b", "p1", "p2")...)
	plan := planSchedule([]*schedObject{o2, o1}, 1, false, &fakeEnv{})
	wantDispatch(t, plan, "a/p3")
	// The slot after that goes to the next object of the group.
	plan = planSchedule([]*schedObject{o2, o1}, 2, false, &fakeEnv{})
	wantDispatch(t, plan, "a/p3", "b/p1")
}

// Groups go in arrival order; each is dispatched completely before the next starts.
func TestPlanGroupsInArrivalOrder(t *testing.T) {
	g2 := obj("lab/b", "g2", 5, nil, pods("b", "p1", "p2")...)
	g1a := obj("lab/a1", "g1", 1, nil, pods("a1", "p1")...)
	g1b := obj("lab/a2", "g1", 9, nil, pods("a2", "p1")...)
	plan := planSchedule([]*schedObject{g2, g1b, g1a}, 100, false, &fakeEnv{})
	wantDispatch(t, plan, "a1/p1", "a2/p1", "b/p1", "b/p2")
}

// A group being dispatched keeps the slots even when an older group became eligible.
func TestPlanCurrentGroupKeepsSlots(t *testing.T) {
	older := obj("lab/old", "old", 1, nil, pods("old", "p1")...)
	current := obj("lab/cur", "cur", 5, nil, pods("cur", "p1:S", "p2")...)
	plan := planSchedule([]*schedObject{older, current}, 1, false, &fakeEnv{})
	wantDispatch(t, plan, "cur/p2")
}

// The next group starts as soon as the current one has nothing left to dispatch,
// without waiting for readiness.
func TestPlanNextGroupDoesNotWaitForReadiness(t *testing.T) {
	g1 := obj("lab/a", "g1", 1, nil, pods("a", "p1:S", "p2:S")...)
	g2 := obj("lab/b", "g2", 2, nil, pods("b", "p1")...)
	plan := planSchedule([]*schedObject{g1, g2}, 1, false, &fakeEnv{})
	wantDispatch(t, plan, "b/p1")
}

// A group with deploy-after starts only when every listed group is complete
// (every pod Ready or failed); until then the others go on.
func TestPlanDependencies(t *testing.T) {
	g1 := obj("lab/a", "g1", 1, nil, pods("a", "p1:D", "p2:S")...)
	g2 := obj("lab/b", "g2", 2, []string{"g1"}, pods("b", "p1")...)
	g3 := obj("lab/c", "g3", 3, nil, pods("c", "p1")...)
	plan := planSchedule([]*schedObject{g1, g2, g3}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "c/p1")
	s := plan.status["lab/b"]
	if s.Reason != laboratoryv1alpha1.WaitForGroup || s.Message != "waiting for group g1" || s.Position != 1 {
		t.Fatalf("status = %+v", s)
	}

	// g1 completes with one failed pod: g2 may start.
	g1 = obj("lab/a", "g1", 1, nil, pods("a", "p1:D", "p2:F")...)
	g2 = obj("lab/b", "g2", 2, []string{"g1"}, pods("b", "p1")...)
	plan = planSchedule([]*schedObject{g1, g2}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "b/p1")
	if _, waiting := plan.status["lab/b"]; waiting {
		t.Fatal("nothing of g2 waits any more")
	}
}

func TestPlanAllListedDependenciesMustBeComplete(t *testing.T) {
	g1 := obj("lab/a", "g1", 1, nil, pods("a", "p1:D")...)
	g2 := obj("lab/b", "g2", 2, nil, pods("b", "p1:S")...)
	g3 := obj("lab/c", "g3", 3, []string{"g1", "g2"}, pods("c", "p1")...)
	plan := planSchedule([]*schedObject{g1, g2, g3}, 10, false, &fakeEnv{})
	wantDispatch(t, plan)
	if m := plan.status["lab/c"].Message; m != "waiting for group g2" {
		t.Fatalf("message = %q", m)
	}
}

func TestPlanUnknownDependencyWaits(t *testing.T) {
	g := obj("lab/a", "g", 1, []string{"ghost"}, pods("a", "p1")...)
	plan := planSchedule([]*schedObject{g}, 10, false, &fakeEnv{})
	wantDispatch(t, plan)
	s := plan.status["lab/a"]
	if s.Reason != laboratoryv1alpha1.WaitForGroup || s.Message != "waiting for group ghost (not known yet)" {
		t.Fatalf("status = %+v", s)
	}
}

// A group that appears later satisfies the dependency once it is complete.
func TestPlanDependencyOnLaterGroupAndCycle(t *testing.T) {
	a := obj("lab/a", "a", 1, []string{"b"}, pods("a", "p1")...)
	b := obj("lab/b", "b", 2, []string{"a"}, pods("b", "p1")...)
	plan := planSchedule([]*schedObject{a, b}, 10, false, &fakeEnv{})
	wantDispatch(t, plan) // a cycle starts nothing, and says why
	if plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitForGroup || plan.status["lab/b"].Reason != laboratoryv1alpha1.WaitForGroup {
		t.Fatalf("status = %+v", plan.status)
	}
	self := obj("lab/s", "s", 1, []string{"s"}, pods("s", "p1")...)
	wantDispatch(t, planSchedule([]*schedObject{self}, 10, false, &fakeEnv{}))
}

// A group of objects with no pods is complete at once.
func TestPlanEmptyGroupIsComplete(t *testing.T) {
	g1 := obj("lab/a", "g1", 1, nil)
	g2 := obj("lab/b", "g2", 2, []string{"g1"}, pods("b", "p1")...)
	wantDispatch(t, planSchedule([]*schedObject{g1, g2}, 10, false, &fakeEnv{}), "b/p1")
}

// Objects without a group are independent and go after all grouped work, one
// object at a time in creation order.
func TestPlanIndependentObjectsGoLast(t *testing.T) {
	i1 := obj("lab/i1", "", 1, nil, pods("i1", "p1", "p2")...)
	g := obj("lab/g", "g", 9, nil, pods("g", "p1")...)
	i0 := obj("lab/i0", "", 0, nil, pods("i0", "p1")...)
	plan := planSchedule([]*schedObject{i1, g, i0}, 100, false, &fakeEnv{})
	wantDispatch(t, plan, "g/p1", "i0/p1", "i1/p1", "i1/p2")
	// An independent object does not wait for a group blocked on a dependency.
	blocked := obj("lab/b", "b", 1, []string{"ghost"}, pods("b", "p1")...)
	plan = planSchedule([]*schedObject{blocked, i1}, 100, false, &fakeEnv{})
	wantDispatch(t, plan, "i1/p1", "i1/p2")
}

// A late object of a group that is already running joins it and goes ahead of
// groups that have not started.
func TestPlanLateObjectJoinsItsGroup(t *testing.T) {
	g1a := obj("lab/a", "g1", 1, nil, pods("a", "p1:D")...)
	g1late := obj("lab/late", "g1", 50, nil, pods("late", "p1")...)
	g2 := obj("lab/b", "g2", 2, nil, pods("b", "p1")...)
	plan := planSchedule([]*schedObject{g1a, g2, g1late}, 1, false, &fakeEnv{})
	wantDispatch(t, plan, "late/p1")
}

// A pod nobody initialised yet is not dispatched, but it keeps its group incomplete.
func TestPlanUninitialisedPods(t *testing.T) {
	g1 := obj("lab/a", "g1", 1, nil, pods("a", "p1:D", "p2:")...)
	g2 := obj("lab/b", "g2", 2, []string{"g1"}, pods("b", "p1")...)
	plan := planSchedule([]*schedObject{g1, g2}, 10, false, &fakeEnv{})
	wantDispatch(t, plan)
	if plan.status["lab/a"].Pending != 1 {
		t.Fatalf("status = %+v", plan.status["lab/a"])
	}
}

func TestPlanSlotsAndUnlimited(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, pods("a", "p1", "p2")...)
	plan := planSchedule([]*schedObject{o}, 0, false, &fakeEnv{})
	wantDispatch(t, plan)
	if plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitInFlightLimit {
		t.Fatalf("status = %+v", plan.status["lab/a"])
	}
	plan = planSchedule([]*schedObject{o}, -5, true, &fakeEnv{})
	wantDispatch(t, plan, "a/p1", "a/p2")
}

// A retried pod goes first, ahead of every group, the oldest request first.
func TestPlanRetryGoesToTheHead(t *testing.T) {
	g1 := obj("lab/a", "g1", 1, nil, pods("a", "p1")...)
	late := obj("lab/z", "g9", 99, nil, pods("z", "r1", "r2")...)
	late.pods[0].retryAt = planEpoch.Add(20 * time.Second)
	late.pods[1].retryAt = planEpoch.Add(10 * time.Second)
	plan := planSchedule([]*schedObject{g1, late}, 2, false, &fakeEnv{})
	wantDispatch(t, plan, "z/r2", "z/r1")
	// With one slot only the oldest retry goes, and nothing else.
	plan = planSchedule([]*schedObject{g1, late}, 1, false, &fakeEnv{})
	wantDispatch(t, plan, "z/r2")
	// A retried pod is dispatched once, not again in the conveyor.
	plan = planSchedule([]*schedObject{g1, late}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "z/r2", "z/r1", "a/p1")
}

func TestPlanResourceCheck(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, pods("a", "p1", "p2")...)
	b := obj("lab/b", "g1", 2, nil, pods("b", "p1")...)
	// No room now: the queue waits at that pod and nothing behind it goes.
	env := &fakeEnv{fits: map[string]fit{"a/p2": fitWait}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan, "a/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason = %q", r)
	}
	if r := plan.status["lab/b"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason of the object behind = %q", r)
	}
	// No node at all.
	plan = planSchedule([]*schedObject{a}, 10, false, &fakeEnv{fits: map[string]fit{"a/p1": fitNoNodes}})
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitNoSchedulableNodes {
		t.Fatalf("reason = %q", r)
	}
	// A pod that fits no node ever is failed; the others go on.
	env = &fakeEnv{fits: map[string]fit{"a/p1": fitNever}}
	plan = planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan, "a/p2", "b/p1")
	if len(plan.failed) != 1 || plan.failed[0].pod.key != "a/p1" {
		t.Fatalf("failed = %+v", plan.failed)
	}
	if env.taken != 2 {
		t.Fatalf("reserved %d pods, want the 2 dispatched", env.taken)
	}
}

func TestPlanImagesMustBeOnTheNodes(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, pods("a", "p1")...)
	a.prepKey = "g/g1"
	b := obj("lab/b", "g2", 2, nil, pods("b", "p1")...)
	env := &fakeEnv{notPrepared: map[string]bool{"g/g1": true}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan)
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("reason = %q", r)
	}
	// A retried pod needs no prepull: its image is on its node already.
	a.pods[0].retryAt = planEpoch
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env), "a/p1", "b/p1")
}

// Queue places count the objects that still have pods to dispatch, the
// dispatchable ones first and the ones waiting for a group after them.
func TestPlanPositions(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, pods("a", "p1")...)
	w := obj("lab/w", "g2", 2, []string{"g1"}, pods("w", "p1")...)
	i := obj("lab/i", "", 3, nil, pods("i", "p1")...)
	plan := planSchedule([]*schedObject{w, i, a}, 0, false, &fakeEnv{})
	pos := func(id string) int32 { return plan.status[id].Position }
	if pos("lab/a") != 1 || pos("lab/i") != 2 || pos("lab/w") != 3 {
		t.Fatalf("positions a=%d i=%d w=%d", pos("lab/a"), pos("lab/i"), pos("lab/w"))
	}
	for _, id := range []string{"lab/a", "lab/i", "lab/w"} {
		if plan.status[id].Length != 3 {
			t.Fatalf("%s length = %d", id, plan.status[id].Length)
		}
	}
	if plan.status["lab/w"].Reason != laboratoryv1alpha1.WaitForGroup || plan.status["lab/i"].Reason != laboratoryv1alpha1.WaitInFlightLimit {
		t.Fatalf("reasons: %+v %+v", plan.status["lab/w"], plan.status["lab/i"])
	}
}

// Mixed kinds share a group: a LabGroup's pods and a Lab's pods are one group.
func TestPlanMixedKindsInOneGroup(t *testing.T) {
	grp := obj("group/team", "g", 1, nil, &schedPod{key: "group/team/vpn", name: "vpn", kind: kindGroupPod, state: qd},
		&schedPod{key: "group/team/gateway", name: "gateway", kind: kindGroupPod, state: qd})
	lab := obj("lab/team/l1", "g", 2, nil, pods("l1", "web")...)
	plan := planSchedule([]*schedObject{lab, grp}, 3, false, &fakeEnv{})
	wantDispatch(t, plan, "group/team/gateway", "group/team/vpn", "l1/web")
}
