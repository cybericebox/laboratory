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
	never       map[string]bool // by pod key: no single node can ever hold it
	noNode      bool            // no schedulable lab node
	taken       int
	pulled      []string        // prepKey of every prepared() call
	tenantFull  map[string]bool // by tenant
	// nodes are the free rooms offered to gangs; nil: no per-node check.
	nodes []nodeRoom
	// empty are the rooms with no lab running; nil: no verdict.
	empty []nodeRoom
	// maxNode is the most one node can give a pod; nil: no per-node verdict.
	maxNode *amount
	// tenantCPU is the CPU quota of a tenant: what it has dispatched (tenantUsed, take adds to it) plus what a gang asks
	// (with the room kept for the tenant added) must stay within it.
	tenantCPU  map[string]int64
	tenantUsed map[string]int64
	// tenantNever marks the tenants whose quota can never hold a gang.
	tenantNever map[string]bool
	// podNode is the node of a dispatched pod by key ("": not on a node yet); a pod not listed is bound to a node that is not
	// in the rooms (nodes are the rooms after it already).
	podNode map[string]string
	// fair is the reservation usage, the clock and the aging threshold; zero: no usage, no aging.
	fair fairness
}

func (f *fakeEnv) fairness() fairness { return f.fair }

func (f *fakeEnv) nodeOf(p *schedPod) string {
	if n, ok := f.podNode[p.key]; ok {
		return n
	}
	return "elsewhere"
}

func (f *fakeEnv) rooms() ([]nodeRoom, bool) { return f.nodes, f.nodes != nil }

// emptyRooms is nil unless a test sets it: no DoesNotFit verdict per node.
func (f *fakeEnv) emptyRooms() []nodeRoom { return f.empty }

func (f *fakeEnv) neverFits(p *schedPod) bool {
	if f.never[p.key] {
		return true
	}
	return f.maxNode != nil && (p.need.cpu > f.maxNode.cpu || p.need.mem > f.maxNode.mem)
}

func (f *fakeEnv) tenantEverFits(p *schedPod) bool { return !f.tenantNever[p.tenant] }

func (f *fakeEnv) tenantFits(p *schedPod) bool {
	if c, ok := f.tenantCPU[p.tenant]; ok && p.need.cpu+f.tenantUsed[p.tenant] > c {
		return false
	}
	return !f.tenantFull[p.tenant]
}
func (f *fakeEnv) prepared(o *schedObject) bool {
	f.pulled = append(f.pulled, o.prepKey)
	return !f.notPrepared[o.prepKey]
}
func (f *fakeEnv) noNodes() bool { return f.noNode }
func (f *fakeEnv) take(p *schedPod) {
	f.taken++
	if f.tenantUsed == nil {
		f.tenantUsed = map[string]int64{}
	}
	f.tenantUsed[p.tenant] += p.need.cpu
}

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

// A gang starts whole or not at all: 3 slots, 5 pods, 2 more in flight: it waits for the slots.
func TestPlanGangWaitsForSlots(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, pods("a", "p1", "p2", "p3", "p4", "p5")...)
	busy := obj("lab/x", "", 2, nil, pods("x", "p1:S", "p2:S")...)
	plan := planSchedule([]*schedObject{o, busy}, 3, false, &fakeEnv{})
	wantDispatch(t, plan)
	st := plan.status["lab/a"]
	if st.Position != 1 || st.Length != 1 || st.Pending != 5 || st.Pods != 5 || st.Reason != laboratoryv1alpha1.WaitInFlightLimit {
		t.Fatalf("status = %+v", st)
	}
	plan = planSchedule([]*schedObject{o, busy}, 5, false, &fakeEnv{})
	wantDispatch(t, plan, "a/p1", "a/p2", "a/p3", "a/p4", "a/p5")
}

// The rest of a partly started group is one gang: the started object first, then the others, all or nothing.
func TestPlanConveyorDoesNotInterleave(t *testing.T) {
	o1 := obj("lab/a", "g", 1, nil, pods("a", "p1:S", "p2:S", "p3")...)
	o2 := obj("lab/b", "g", 2, nil, pods("b", "p1", "p2")...)
	plan := planSchedule([]*schedObject{o2, o1}, 2, false, &fakeEnv{})
	wantDispatch(t, plan)
	plan = planSchedule([]*schedObject{o2, o1}, 3, false, &fakeEnv{})
	wantDispatch(t, plan, "a/p3", "b/p1", "b/p2")
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

func TestPlanResourceCheck(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, pods("a", "p1", "p2")...)
	a.pods[1].need = amount{cpu: 100, mem: 2 * gi}
	b := obj("lab/b", "g1", 2, nil, pods("b", "p1")...)
	// No room now for a pod of a: the gang (a and b) stays queued (InsufficientResources).
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan)
	// A gang of another group goes past it (backfill).
	c := obj("lab/c", "g2", 3, nil, pods("c", "p1")...)
	plan = planSchedule([]*schedObject{a, b, c}, 10, false, env)
	wantDispatch(t, plan, "c/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason = %q", r)
	}
	if _, waiting := plan.status["lab/c"]; waiting {
		t.Fatalf("c went past the gang: %+v", plan.status["lab/c"])
	}
	// No node at all.
	plan = planSchedule([]*schedObject{a}, 10, false, &fakeEnv{noNode: true})
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitNoSchedulableNodes {
		t.Fatalf("reason = %q", r)
	}
	// A pod that fits no node ever fails its whole gang: nothing of it starts. Another group goes on.
	env = &fakeEnv{never: map[string]bool{"a/p1": true}}
	plan = planSchedule([]*schedObject{a, b, c}, 10, false, env)
	wantDispatch(t, plan, "c/p1")
	if len(plan.failed) != 3 || plan.failed[0].pod.key != "a/p1" || plan.failed[0].reason != laboratoryv1alpha1.FailureDoesNotFit {
		t.Fatalf("failed = %+v", plan.failed)
	}
	if env.taken != 1 {
		t.Fatalf("reserved %d pods, want the 1 dispatched", env.taken)
	}
}

func TestPlanImagesMustBeOnTheNodes(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, pods("a", "p1")...)
	a.prepKey = "g/g1"
	b := obj("lab/b", "g2", 2, nil, pods("b", "p1")...)
	env := &fakeEnv{notPrepared: map[string]bool{"g/g1": true}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	// The images gate their own group only: the next group goes on.
	wantDispatch(t, plan, "b/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("reason = %q", r)
	}
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

// A tenant at its quota waits with the reason TenantQuota and does not hold up the others.
func TestTenantAtQuotaWaitsWithoutBlockingOthers(t *testing.T) {
	a := obj("lab/ns/a", "", 0, nil, pods("lab/ns/a", "web", "db")...)
	b := obj("lab/ns/b", "", 1, nil, pods("lab/ns/b", "web")...)
	for _, p := range a.pods {
		p.tenant = "full"
	}
	for _, p := range b.pods {
		p.tenant = "free"
	}
	plan := planSchedule([]*schedObject{a, b}, 10, false, &fakeEnv{tenantFull: map[string]bool{"full": true}})
	if len(plan.dispatch) != 1 || plan.dispatch[0].key != "lab/ns/b/web" {
		t.Fatalf("only the other tenant's pod starts: %+v", plan.dispatch)
	}
	st := plan.status["lab/ns/a"]
	if st.Reason != laboratoryv1alpha1.WaitTenantQuota || st.Pending != 2 {
		t.Fatalf("status of the held object: %+v", st)
	}
	if _, waiting := plan.status["lab/ns/b"]; waiting {
		t.Fatal("the free tenant's object has nothing left to wait for")
	}
}

// The quota counts the requests of the dispatched pods of the tenant, and each new
// dispatch in the same pass.
func TestClusterEnvEnforcesTenantQuota(t *testing.T) {
	ten := &laboratoryv1alpha1.Tenant{Spec: laboratoryv1alpha1.TenantSpec{Quota: &laboratoryv1alpha1.TenantQuota{CPU: "1", Memory: "1Gi"}}}
	run := &schedPod{key: "ns/l/run", kind: kindDevicePod, state: sd, tenant: "t", need: amount{cpu: 400, mem: 100 << 20}}
	q1 := &schedPod{key: "ns/l/q1", kind: kindDevicePod, state: qd, tenant: "t", need: amount{cpu: 500, mem: 100 << 20}}
	q2 := &schedPod{key: "ns/l/q2", kind: kindDevicePod, state: qd, tenant: "t", need: amount{cpu: 500, mem: 100 << 20}}
	other := &schedPod{key: "ns/o/x", kind: kindDevicePod, state: qd, tenant: "nobody", need: amount{cpu: 99000, mem: 1 << 40}}
	env := &clusterEnv{s: &Scheduler{}, snap: &clusterView{tenants: map[string]*laboratoryv1alpha1.Tenant{"t": ten}},
		objs: []*schedObject{{pods: []*schedPod{run, q1, q2, other}}}}
	if !env.tenantFits(q1) {
		t.Fatal("400m running + 500m fits 1 CPU")
	}
	env.take(q1)
	if env.tenantFits(q2) {
		t.Fatal("900m reserved + 500m is over 1 CPU")
	}
	if !env.tenantFits(other) {
		t.Fatal("a tenant without a Tenant object or quota has no limit")
	}
	// Memory limit too.
	big := &schedPod{key: "ns/l/big", kind: kindDevicePod, state: qd, tenant: "t", need: amount{cpu: 1, mem: 2 << 30}}
	if env.tenantFits(big) {
		t.Fatal("memory over the quota")
	}
}

// The service pods of a new team go ahead of labs that wait for room: a burst of labs that does
// not fit must not keep a new LabGroup from starting, and the labs stay in their own order.
func TestPlanGroupPodsGoAheadOfQueuedLabs(t *testing.T) {
	old := obj("lab/old/l1", "", 1, nil, pods("l1", "web")...)
	old2 := obj("lab/old/l2", "", 2, nil, pods("l2", "web")...)
	grp := obj("group/new", "", 3, nil, &schedPod{key: "group/new/vpn", name: "vpn", kind: kindGroupPod, state: qd},
		&schedPod{key: "group/new/gateway", name: "gateway", kind: kindGroupPod, state: qd})
	// the labs do not fit; the group pods do
	old.pods[0].need, old2.pods[0].need = amount{cpu: 100, mem: 2 * gi}, amount{cpu: 100, mem: 2 * gi}
	grp.pods[0].need, grp.pods[1].need = amount{cpu: 100, mem: gi / 4}, amount{cpu: 10, mem: gi / 8}
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}}
	plan := planSchedule([]*schedObject{old, old2, grp}, 10, false, env)
	wantDispatch(t, plan, "group/new/gateway", "group/new/vpn")
	if r := plan.status["lab/old/l1"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("the lab at the head waits for room, reason = %q", r)
	}
	if _, waiting := plan.status["group/new"]; waiting {
		t.Fatal("the group has nothing left to wait for")
	}
	// the group pods are still checked: no room for them, they stay queued and the lab goes past them
	grp.pods[1].need = amount{cpu: 10, mem: 3 * gi}
	env = &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}}
	plan = planSchedule([]*schedObject{old, grp}, 10, false, env)
	wantDispatch(t, plan, "l1/web")
	if r := plan.status["group/new"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason = %q", r)
	}
	// a lab that does not fit stays queued and the next lab goes past it (backfill)
	old2.pods[0].need = amount{cpu: 100, mem: gi / 2}
	env = &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}}
	plan = planSchedule([]*schedObject{old, old2}, 10, false, env)
	wantDispatch(t, plan, "l2/web")
	// a group that depends on another group still waits for it
	dep := obj("group/dep", "d", 4, []string{"other"}, &schedPod{key: "group/dep/vpn", name: "vpn", kind: kindGroupPod, state: qd})
	plan = planSchedule([]*schedObject{dep}, 10, false, &fakeEnv{})
	wantDispatch(t, plan)
	if plan.status["group/dep"].Reason != laboratoryv1alpha1.WaitForGroup {
		t.Fatalf("reason = %q", plan.status["group/dep"].Reason)
	}
}

// The VPN and gateway pods of a tenant's groups request real resources: they count against the quota like
// lab pods (what GetCapacity reports as reserved must never exceed the quota), and a new group waits for
// room in the quota too.
func TestTenantQuotaCountsGroupPods(t *testing.T) {
	ten := &laboratoryv1alpha1.Tenant{Spec: laboratoryv1alpha1.TenantSpec{Quota: &laboratoryv1alpha1.TenantQuota{CPU: "700m", Memory: "4Gi"}}}
	vpn := &schedPod{key: "group/g1/vpn", lookup: "g1/vpn", kind: kindGroupPod, state: sd, tenant: "t", need: amount{cpu: 100, mem: 320 << 20}}
	gw := &schedPod{key: "group/g1/gateway", lookup: "g1/gateway", kind: kindGroupPod, state: sd, tenant: "t", need: amount{cpu: 10, mem: 32 << 20}}
	lab := &schedPod{key: "ns/l/web", kind: kindDevicePod, state: qd, tenant: "t", need: amount{cpu: 600, mem: 64 << 20}}
	newVPN := &schedPod{key: "group/g2/vpn", lookup: "g2/vpn", kind: kindGroupPod, state: qd, tenant: "t", need: amount{cpu: 100, mem: 320 << 20}}
	env := &clusterEnv{s: &Scheduler{}, snap: &clusterView{tenants: map[string]*laboratoryv1alpha1.Tenant{"t": ten}},
		objs: []*schedObject{{pods: []*schedPod{vpn, gw, lab, newVPN}}}}
	if env.tenantFits(lab) {
		t.Fatal("110m of group pods + 600m is over 700m: a lab must not slip past the group overhead")
	}
	lab.need.cpu = 500
	if !env.tenantFits(lab) {
		t.Fatal("110m + 500m fits 700m")
	}
	env.take(lab)
	if env.tenantFits(newVPN) {
		t.Fatal("a new group's VPN pod waits when the quota is used up (610m + 100m > 700m)")
	}
}

// The scheduler sizes a group pod from the chart's group pod resources.
func TestGroupPodNeedFollowsTheChart(t *testing.T) {
	s := &Scheduler{}
	if n := s.groupPodNeed("vpn", &laboratoryv1alpha1.LabGroup{}); n.Cpu().MilliValue() != 100 || n.Memory().Value() != 320<<20 {
		t.Fatalf("vpn %v", n)
	}
	if n := s.groupPodNeed("gateway", &laboratoryv1alpha1.LabGroup{}); n.Cpu().MilliValue() != 10 || n.Memory().Value() != 32<<20 {
		t.Fatalf("gateway %v", n)
	}
	if s.groupPodNeed("other", &laboratoryv1alpha1.LabGroup{}) != nil {
		t.Fatal("an unknown pod has no need")
	}
}

func sized(o *schedObject, cpu, mem int64) *schedObject {
	for _, p := range o.pods {
		p.need = amount{cpu: cpu, mem: mem}
	}
	return o
}

// Inside a group the larger lab goes first (so the big ones are placed while the nodes are empty); an explicit priority
// wins over size; objects of different groups keep the group order whatever their size.
func TestPlanLargerLabsFirstWithinTheSamePriority(t *testing.T) {
	small := sized(obj("lab/s", "g", 1, nil, pods("s", "web")...), 100, 64<<20)
	big := sized(obj("lab/b", "g", 2, nil, pods("b", "web")...), 1000, 1<<30)
	mid := sized(obj("lab/m", "g", 3, nil, pods("m", "web")...), 500, 1<<30)
	plan := planSchedule([]*schedObject{small, big, mid}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "b/web", "m/web", "s/web")

	// an explicit priority wins over size
	small.priority = 5
	plan = planSchedule([]*schedObject{small, big, mid}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "s/web", "b/web", "m/web")

	// groups keep their order and dependencies: a later group's big lab never goes before an earlier group's small one
	g1 := sized(obj("lab/g1", "g1", 1, nil, pods("g1", "web")...), 100, 64<<20)
	g2 := sized(obj("lab/g2", "g2", 2, nil, pods("g2", "web")...), 4000, 8<<30)
	plan = planSchedule([]*schedObject{g2, g1}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "g1/web", "g2/web")
	g2.after = []string{"g1"}
	plan = planSchedule([]*schedObject{g2, g1}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "g1/web")

	// independent labs are ordered the same way
	i1 := sized(obj("lab/i1", "", 1, nil, pods("i1", "web")...), 100, 64<<20)
	i2 := sized(obj("lab/i2", "", 2, nil, pods("i2", "web")...), 900, 64<<20)
	plan = planSchedule([]*schedObject{i1, i2}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "i2/web", "i1/web")
}

// An object that is being dispatched keeps going before a larger one starts, and a big lab that does not fit lets the
// small ones fill the gaps without losing its place for the next pass.
func TestPlanBackfillKeepsTheBigLabFirst(t *testing.T) {
	big := sized(obj("lab/b", "g", 1, nil, pods("b", "web")...), 4000, 8<<30)
	small := sized(obj("lab/s", "g2", 2, nil, pods("s", "web")...), 100, 64<<20)
	plan := planSchedule([]*schedObject{small, big}, 10, false, &fakeEnv{nodes: []nodeRoom{room("n1", 2000, 4<<30)}})
	wantDispatch(t, plan, "s/web")
	if plan.status["lab/b"].Reason != laboratoryv1alpha1.WaitInsufficient || plan.status["lab/b"].Position != 1 {
		t.Fatalf("the big lab stays at the head: %+v", plan.status["lab/b"])
	}
	// room appears: the big lab goes first again
	plan = planSchedule([]*schedObject{small, big}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "b/web", "s/web")
	started := sized(obj("lab/st", "g", 3, nil, pods("st", "p1:D", "p2")...), 10, 1<<20)
	plan = planSchedule([]*schedObject{big, started}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "st/p2", "b/web")
}

// reserved builds a lab of one queued pod under a reservation (empty: unreserved), of the given memory and 100m.
func reserved(id, reservation string, arrival int, mem int64) *schedObject {
	o := obj("lab/"+id, "", arrival, nil, need(pods(id, "p1"), 100, mem)...)
	o.reservation = reservation
	return o
}

func gangKeys(gangs []*gang) []string {
	var out []string
	for _, g := range gangs {
		out = append(out, g.key)
	}
	return out
}

// Of two reservations with waiting work, the one with the lower satisfied share goes first: A has used 50 % of its memory,
// B 10 %, so B's lab takes the only room although A's came first. Memory decides first, CPU only on equal memory shares.
func TestLowestShareGoesFirst(t *testing.T) {
	one := []nodeRoom{room("n1", 4000, gi)}
	usage := map[string]reservationUsage{
		"A": {used: amount{cpu: 100, mem: 5 * gi}, amount: amount{cpu: 1000, mem: 10 * gi}},
		"B": {used: amount{cpu: 900, mem: 1 * gi}, amount: amount{cpu: 1000, mem: 10 * gi}},
	}
	a, b := reserved("a", "A", 1, gi), reserved("b", "B", 2, gi)
	plan := planSchedule([]*schedObject{a, b}, 10, false, &fakeEnv{nodes: one, empty: one, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "b/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("a reason = %q", r)
	}
	// equal memory shares: the lower CPU share goes first
	usage["B"] = reservationUsage{used: amount{cpu: 900, mem: 5 * gi}, amount: amount{cpu: 1000, mem: 10 * gi}}
	plan = planSchedule([]*schedObject{a, b}, 10, false, &fakeEnv{nodes: one, empty: one, fair: fairness{usage: usage}})
	wantDispatch(t, plan, "a/p1")

	// An admitted gang keeps its place ahead of every new one, whatever the shares; a reservation that reserves nothing
	// (invalid) goes after the valid ones.
	started := &gang{key: "s", arrival: planEpoch.Add(9 * time.Second), reservation: "A",
		objs: []*schedObject{obj("lab/s", "", 9, nil, pods("s", "p1:D", "p2")...)}}
	ga := &gang{key: "a", arrival: planEpoch.Add(time.Second), reservation: "A"}
	gb := &gang{key: "b", arrival: planEpoch.Add(2 * time.Second), reservation: "B"}
	gz := &gang{key: "z", arrival: planEpoch, reservation: "Z"}
	usage = map[string]reservationUsage{
		"A": {used: amount{mem: 5 * gi}, amount: amount{cpu: 1000, mem: 10 * gi}},
		"B": {used: amount{mem: 1 * gi}, amount: amount{cpu: 1000, mem: 10 * gi}},
		"Z": {},
	}
	if got := gangKeys(gangOrder([]*gang{gz, ga, gb, started}, usage)); !same(got, []string{"s", "b", "a", "z"}) {
		t.Fatalf("order = %v, want [s b a z]", got)
	}
}

// Unreserved gangs come after every reserved one, even when they arrived first; among themselves they keep the conveyor
// order.
func TestUnreservedAfterReserved(t *testing.T) {
	one := []nodeRoom{room("n1", 4000, gi)}
	u, r := reserved("u", "", 1, gi), reserved("r", "R", 2, gi)
	plan := planSchedule([]*schedObject{u, r}, 10, false, &fakeEnv{nodes: one, empty: one})
	wantDispatch(t, plan, "r/p1")
	if p := plan.status["lab/u"]; p.Reason != laboratoryv1alpha1.WaitInsufficient || p.Position != 1 {
		t.Fatalf("u status = %+v", p)
	}
	// with room for both, both go, the reserved first
	two := []nodeRoom{room("n1", 4000, 2*gi)}
	wantDispatch(t, planSchedule([]*schedObject{u, r}, 10, false, &fakeEnv{nodes: two, empty: two}), "r/p1", "u/p1")

	gu1 := &gang{key: "u1", arrival: planEpoch}
	gu2 := &gang{key: "u2", arrival: planEpoch.Add(-time.Second)}
	gr := &gang{key: "r", arrival: planEpoch.Add(time.Hour), reservation: "R"}
	if got := gangKeys(gangOrder([]*gang{gu1, gu2, gr}, nil)); !same(got, []string{"r", "u1", "u2"}) {
		t.Fatalf("order = %v, want [r u1 u2]", got)
	}
}

// With equal shares (no usage counted yet, so every share is 0) arrival decides, then the tenant, then the key.
func TestEqualShareByArrival(t *testing.T) {
	one := []nodeRoom{room("n1", 4000, gi)}
	late, early := reserved("late", "A", 2, gi), reserved("early", "B", 1, gi)
	wantDispatch(t, planSchedule([]*schedObject{late, early}, 10, false, &fakeEnv{nodes: one, empty: one}), "early/p1")

	g1 := &gang{key: "t2/x", tenant: "t2", arrival: planEpoch, reservation: "A"}
	g2 := &gang{key: "t1/y", tenant: "t1", arrival: planEpoch, reservation: "B"}
	g3 := &gang{key: "t1/x", tenant: "t1", arrival: planEpoch, reservation: "C"}
	g4 := &gang{key: "t0/z", tenant: "t0", arrival: planEpoch.Add(time.Second), reservation: "D"}
	if got := gangKeys(gangOrder([]*gang{g4, g1, g2, g3}, map[string]reservationUsage{})); !same(got, []string{"t1/x", "t1/y", "t2/x", "t0/z"}) {
		t.Fatalf("order = %v", got)
	}
}
