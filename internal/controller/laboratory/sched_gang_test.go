package laboratory

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

const gi = int64(1) << 30

// need gives every pod the same request.
func need(ps []*schedPod, cpu, mem int64) []*schedPod {
	for _, p := range ps {
		p.need = amount{cpu: cpu, mem: mem}
	}
	return ps
}

func room(name string, cpu, mem int64) nodeRoom { return nodeRoom{Name: name, CPU: cpu, Memory: mem} }

func TestFitsGangDecreasing(t *testing.T) {
	rooms := []nodeRoom{room("a", 4000, 4*gi), room("b", 4000, 2*gi)}
	// First-fit in input order would put the 1Gi pods on a and strand the 3Gi pod; decreasing keeps it whole.
	needs := []podNeed{{"s1", 100, gi}, {"s2", 100, gi}, {"big", 100, 3 * gi}}
	left, ok := fitsGang(rooms, needs)
	if !ok {
		t.Fatal("must fit")
	}
	if left[0].Memory != 0 || left[1].Memory != gi {
		t.Fatalf("left = %+v", left)
	}
	if rooms[0].Memory != 4*gi {
		t.Fatal("the input must not change")
	}
	if _, ok := fitsGang(rooms, append(needs, podNeed{"x", 1, 2 * gi})); ok {
		t.Fatal("no room left for one more")
	}
}

func TestFitsGangCPU(t *testing.T) {
	rooms := []nodeRoom{room("a", 1000, 8*gi)}
	if _, ok := fitsGang(rooms, []podNeed{{"a", 600, gi}, {"b", 600, gi}}); ok {
		t.Fatal("CPU does not add up")
	}
}

func TestSubtractFromEmptiest(t *testing.T) {
	rooms := []nodeRoom{room("a", 4000, 2*gi), room("b", 4000, 4*gi)}
	out := subtractFromEmptiest(rooms, []amount{{cpu: 1000, mem: gi}})
	if out[1].Memory != 3*gi || out[0].Memory != 2*gi {
		t.Fatalf("out = %+v", out)
	}
}

func TestGangAllOrNothing(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1", "p2", "p3"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason = %q", r)
	}
	env = &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 3*gi)}}
	plan = planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan, "a/p1", "a/p2", "a/p3")
}

func TestGangWaitsWhenOnlyAggregateFits(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi+gi/2)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi), room("n2", 4000, gi)}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason = %q", r)
	}
}

func TestBackfillAfterBlockedGang(t *testing.T) {
	big := obj("lab/big", "g1", 1, nil, need(pods("big", "p1", "p2"), 100, 2*gi)...)
	small := obj("lab/small", "g2", 2, nil, need(pods("small", "p1"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 3*gi)}}
	plan := planSchedule([]*schedObject{big, small}, 10, false, env)
	wantDispatch(t, plan, "small/p1")
	if r := plan.status["lab/big"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("reason = %q", r)
	}
}

// The room one gang takes is gone for the next one in the same pass.
func TestGangRoomIsDeducted(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1"), 100, 2*gi)...)
	b := obj("lab/b", "g2", 2, nil, need(pods("b", "p1"), 100, 2*gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 3*gi)}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan, "a/p1")
}

func TestStartedPartOfGangIsNotRequeued(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1:S", "p2:D", "p3", "p4"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan, "a/p3", "a/p4")
}

// A gang spans the labs of its deploy group.
func TestGangSpansTheGroup(t *testing.T) {
	a := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi)...)
	b := obj("lab/b", "g", 2, nil, need(pods("b", "p1"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env))
	env = &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env), "a/p1", "b/p1")
}

func TestGangNeedsSlotsForAllPods(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, pods("a", "p1", "p2", "p3")...)
	busy := obj("lab/x", "", 2, nil, pods("x", "p1:S")...)
	plan := planSchedule([]*schedObject{o, busy}, 2, false, &fakeEnv{})
	wantDispatch(t, plan)
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInFlightLimit {
		t.Fatalf("reason = %q", r)
	}
	wantDispatch(t, planSchedule([]*schedObject{o, busy}, 3, false, &fakeEnv{}), "a/p1", "a/p2", "a/p3")
}

// A gang larger than the whole in-flight limit would wait for ever: it goes in slices of the free slots.
func TestGangLargerThanLimitGoesInSlices(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, pods("a", "p1", "p2", "p3")...)
	wantDispatch(t, planSchedule([]*schedObject{o}, 2, false, &fakeEnv{}), "a/p1", "a/p2")
	wantDispatch(t, planSchedule([]*schedObject{o}, 0, false, &fakeEnv{}))
}

// A gang bigger than the empty capacity is not failed for good: it waits with DoesNotFit.
func TestGangTooBigForEmptyLabWaits(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1", "p2"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 4*gi)}, fits: map[string]fit{"gang/g": fitNever}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if len(plan.failed) != 0 || plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("failed %d, status %+v", len(plan.failed), plan.status["lab/a"])
	}
}

// A pod bigger than every single node fails for good, though the nodes together could hold it.
func TestGangDoesNotFitOnEmptyNodes(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi+gi/2)...)
	empty := []nodeRoom{room("n1", 4000, gi), room("n2", 4000, gi)}
	env := &fakeEnv{nodes: empty, empty: empty, maxNode: &amount{cpu: 4000, mem: gi}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if len(plan.failed) != 1 || plan.failed[0].reason != laboratoryv1alpha1.FailureDoesNotFit ||
		plan.failed[0].message != "the pod a/p1 requests more than any single node can give" {
		t.Fatalf("failed = %+v", plan.failed)
	}
}

func TestPodOver5GiOnTwo4GiNodesFailsForGood(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, 5*gi)...)
	empty := []nodeRoom{room("n1", 4000, 4*gi), room("n2", 4000, 4*gi)}
	plan := planSchedule([]*schedObject{o}, 10, false, &fakeEnv{nodes: empty, empty: empty, maxNode: &amount{cpu: 4000, mem: 4 * gi}})
	if len(plan.failed) != 1 {
		t.Fatalf("failed = %+v", plan.failed)
	}
}

// Gang of three pods that fit the empty nodes one by one but not together on them: it waits whole.
func TestGangThatFitsNoEmptyNodeCombinationWaits(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1", "p2", "p3"), 100, gi)...)
	empty := []nodeRoom{room("n1", 4000, gi), room("n2", 4000, gi)}
	plan := planSchedule([]*schedObject{o}, 10, false, &fakeEnv{nodes: empty, empty: empty, maxNode: &amount{cpu: 4000, mem: gi}})
	wantDispatch(t, plan)
	if len(plan.failed) != 0 || plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("failed %d, status %+v", len(plan.failed), plan.status["lab/a"])
	}
}

// A cordoned node or a short rollout does not fail a gang for good.
func TestGangDoesNotFitRecoversWhenNodeReturns(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1", "p2"), 100, gi)...)
	one := []nodeRoom{room("n1", 4000, gi)}
	plan := planSchedule([]*schedObject{o}, 10, false, &fakeEnv{nodes: one, empty: one})
	wantDispatch(t, plan)
	if plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("status = %+v", plan.status["lab/a"])
	}
	two := []nodeRoom{room("n1", 4000, gi), room("n2", 4000, gi)}
	plan = planSchedule([]*schedObject{o}, 10, false, &fakeEnv{nodes: two, empty: two})
	wantDispatch(t, plan, "a/p1", "a/p2")
}

// Only one pod that exceeds every node fails for good, and it takes its gang with it.
func TestSinglePodOverAllocatableStaysFailed(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1", "p2"), 100, gi)...)
	env := &fakeEnv{fits: map[string]fit{"a/p2": fitNever}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if len(plan.failed) != 2 || plan.failed[0].reason != laboratoryv1alpha1.FailureDoesNotFit {
		t.Fatalf("failed = %+v", plan.failed)
	}
}

// Images are pulled while the gang waits for room, and the status names the first thing that holds it:
// TenantQuota, then PreparingImages, then InsufficientResources.
func TestWaitingGangPullsImagesAndStatusPriority(t *testing.T) {
	mk := func() *schedObject {
		o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, 2*gi)...)
		o.prepKey = "g/g"
		return o
	}
	small := []nodeRoom{room("n1", 4000, gi)}
	env := &fakeEnv{nodes: small}
	plan := planSchedule([]*schedObject{mk()}, 10, false, env)
	if len(env.pulled) == 0 || plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("pulled %v, status %+v", env.pulled, plan.status["lab/a"])
	}
	env = &fakeEnv{nodes: small, notPrepared: map[string]bool{"g/g": true}}
	if r := planSchedule([]*schedObject{mk()}, 10, false, env).status["lab/a"].Reason; r != laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("reason = %q", r)
	}
	o := mk()
	o.pods[0].tenant = "t"
	env = &fakeEnv{nodes: small, notPrepared: map[string]bool{"g/g": true}, tenantFull: map[string]bool{"t": true}}
	if r := planSchedule([]*schedObject{o}, 10, false, env).status["lab/a"].Reason; r != laboratoryv1alpha1.WaitTenantQuota {
		t.Fatalf("reason = %q", r)
	}
}

// The room of the rest of a gang that has pods out is kept: a later gang does not take it, even when the rest is held
// for room itself.
func TestAdmittedGangRemainderHoldsRoomAcrossTicks(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1:S", "p2", "p3", "p4"), 100, gi)...)
	b := obj("lab/b", "g2", 2, nil, need(pods("b", "p1"), 100, gi)...)
	// a needs 3Gi more and 2Gi is free: it waits, and its share is not b's.
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env))
	// With 4Gi a goes, and the spare Gi is b's.
	env = &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 4*gi)}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env), "a/p2", "a/p3", "a/p4", "b/p1")
}

// A remainder whose tenant is at the quota cannot dispatch, so it holds no room.
func TestHeldRemainderReleasedWhenTenantAtQuota(t *testing.T) {
	held := need(pods("a", "p1:S", "p2", "p3"), 100, gi)
	for _, p := range held {
		p.tenant = "t"
	}
	a := obj("lab/a", "g1", 1, nil, held...)
	b := obj("lab/b", "g2", 2, nil, need(pods("b", "p1"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}, tenantFull: map[string]bool{"t": true}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env), "b/p1")
}

// After an operator restart the images of a started gang are asked again; its room is kept meanwhile.
func TestHeldRemainderKeptWhileImagesAreAskedAgain(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1:S", "p2", "p3"), 100, gi)...)
	a.prepKey = "g/g1"
	b := obj("lab/b", "g2", 2, nil, need(pods("b", "p1"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 2*gi)}, notPrepared: map[string]bool{"g/g1": true}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env))
}

// A remainder that does not fit even the empty nodes holds no room: another tenant's lab goes.
func TestHeldRemainderReleasedWhenItFitsNoEmptyNodes(t *testing.T) {
	held := need(pods("a", "p1:D", "p2", "p3"), 100, gi)
	for _, p := range held {
		p.tenant = "t1"
	}
	a := obj("lab/a", "g1", 1, nil, held...)
	b := obj("lab/b", "g2", 2, nil, need(pods("b", "p1"), 100, gi/2)...)
	b.pods[0].tenant = "t2"
	free := []nodeRoom{room("n1", 4000, gi)}
	env := &fakeEnv{nodes: free, empty: free, maxNode: &amount{cpu: 4000, mem: gi}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan, "b/p1")
	if plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("status = %+v", plan.status["lab/a"])
	}
}

// A gang waiting for its tenant's quota still gets its images pulled.
func TestTenantHeldGangPullsImages(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi)...)
	o.pods[0].tenant = "t"
	o.prepKey = "g/g"
	env := &fakeEnv{tenantFull: map[string]bool{"t": true}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	if len(env.pulled) == 0 || plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitTenantQuota {
		t.Fatalf("pulled %v, status %+v", env.pulled, plan.status["lab/a"])
	}
}

// Groups of the same name and arrival in two tenants keep a fixed order whatever the input order is.
func TestSameNamedGroupsOrderIsDeterministic(t *testing.T) {
	mk := func(id, tenant string) *schedObject {
		o := obj("lab/"+id, "g", 1, nil, need(pods(id, "p1"), 100, gi)...)
		o.pods[0].tenant = tenant
		return o
	}
	for i := 0; i < 20; i++ {
		a, b := mk("a", "t1"), mk("b", "t2")
		in := []*schedObject{a, b}
		if i%2 == 1 {
			in = []*schedObject{b, a}
		}
		wantDispatch(t, planSchedule(in, 10, false, &fakeEnv{}), "a/p1", "b/p1")
	}
}

// The room kept for a tenant's remainder counts against its quota: another gang of the tenant cannot use it.
func TestHeldRemainderCountsTowardQuota(t *testing.T) {
	held := need(pods("a", "p1:S", "p2", "p3"), 100, gi)
	for _, p := range held {
		p.tenant = "t"
	}
	a := obj("lab/a", "g1", 1, nil, held...)
	mk := func(id string, n int) *schedObject {
		names := make([]string, n)
		for i := range names {
			names[i] = "p" + string(rune('1'+i))
		}
		o := obj("lab/"+id, id, 2, nil, need(pods(id, names...), 100, gi)...)
		for _, p := range o.pods {
			p.tenant = "t"
		}
		return o
	}
	// the quota holds 300m: 200m are kept for a's remainder, so one more pod fits and two do not
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 40000, 40*gi)}, tenantCPU: map[string]int64{"t": 300}}
	plan := planSchedule([]*schedObject{a, mk("b", 2)}, 10, false, env)
	if r := plan.status["lab/b"].Reason; r != laboratoryv1alpha1.WaitTenantQuota {
		t.Fatalf("reason = %q", r)
	}
	env = &fakeEnv{nodes: []nodeRoom{room("n1", 40000, 40*gi)}, tenantCPU: map[string]int64{"t": 300}}
	wantDispatch(t, planSchedule([]*schedObject{a, mk("c", 1)}, 10, false, env), "a/p2", "a/p3", "c/p1")
}

func TestGangsNeverSpanTenants(t *testing.T) {
	a := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi)...)
	b := obj("lab/b", "g", 2, nil, need(pods("b", "p1"), 100, gi)...)
	a.pods[0].tenant, b.pods[0].tenant = "t1", "t2"
	// one gang would need 2Gi; two gangs go one after the other
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}}
	wantDispatch(t, planSchedule([]*schedObject{a, b}, 10, false, env), "a/p1")
}

func TestOtherTenantNotFailedByQuota(t *testing.T) {
	a := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi)...)
	b := obj("lab/b", "g", 2, nil, need(pods("b", "p1"), 100, gi)...)
	a.pods[0].tenant, b.pods[0].tenant = "t1", "t2"
	env := &fakeEnv{tenantNever: map[string]bool{"t1": true}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan, "b/p1")
	if len(plan.failed) != 1 || plan.failed[0].pod.key != "a/p1" {
		t.Fatalf("failed = %+v", plan.failed)
	}
}

func TestGangOfZeroNeedFitsNoRooms(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, pods("a", "p1")...)
	plan := planSchedule([]*schedObject{o}, 10, false, &fakeEnv{nodes: []nodeRoom{}})
	wantDispatch(t, plan, "a/p1")
}

func TestGangLargerThanTheTenantQuotaFails(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, gi)...)
	o.pods[0].tenant = "t"
	plan := planSchedule([]*schedObject{o}, 10, false, &fakeEnv{tenantNever: map[string]bool{"t": true}})
	wantDispatch(t, plan)
	if len(plan.failed) != 1 || plan.failed[0].reason != laboratoryv1alpha1.FailureTenantQuota {
		t.Fatalf("failed = %+v", plan.failed)
	}
}

func TestSubtractCarriesTheExcessToTheNextRoom(t *testing.T) {
	rooms := []nodeRoom{room("a", 4000, 3*gi), room("b", 4000, 2*gi)}
	out := subtractFromEmptiest(rooms, []amount{{cpu: 100, mem: 4 * gi}})
	if out[0].Memory != 0 || out[1].Memory != gi {
		t.Fatalf("out = %+v", out)
	}
}

// CPU still owed after the chosen room ran out of it is taken from a room that has CPU.
func TestSubtractContinuesToRoomWithCPU(t *testing.T) {
	rooms := []nodeRoom{room("a", 0, 2*gi), room("b", 1000, 0)}
	out := subtractFromEmptiest(rooms, []amount{{cpu: 500, mem: 0}})
	if out[1].CPU != 500 || out[0].CPU != 0 {
		t.Fatalf("out = %+v", out)
	}
	out = subtractFromEmptiest(rooms, []amount{{cpu: 500, mem: gi}})
	if out[1].CPU != 500 || out[0].Memory != gi {
		t.Fatalf("out = %+v", out)
	}
}

// deploy-after names a group of the same tenant: a group of that name in another tenant does not satisfy it or block it.
func TestDeployAfterIsTenantScoped(t *testing.T) {
	other := obj("lab/x", "g1", 1, nil, pods("x", "p1:S")...) // g1 of tenant t2, not complete
	other.pods[0].tenant = "t2"
	mine := obj("lab/a", "g1", 1, nil, pods("a", "p1:D")...) // g1 of tenant t1, complete
	mine.pods[0].tenant = "t1"
	next := obj("lab/b", "g2", 2, []string{"g1"}, pods("b", "p1")...)
	next.pods[0].tenant = "t1"
	plan := planSchedule([]*schedObject{other, mine, next}, 10, false, &fakeEnv{})
	wantDispatch(t, plan, "b/p1")
	// a tenant without its own g1 is not released by the g1 of another tenant
	next2 := obj("lab/c", "g2", 2, []string{"g1"}, pods("c", "p1")...)
	next2.pods[0].tenant = "t3"
	plan = planSchedule([]*schedObject{other, mine, next2}, 10, false, &fakeEnv{})
	wantDispatch(t, plan)
	if m := plan.status["lab/c"].Message; m != "waiting for group g1 (not known yet)" {
		t.Fatalf("message = %q", m)
	}
}

// Two admitted gangs whose remainders do not fit the free room together never block each other: the first by arrival
// takes the room, the next waits behind it and keeps what is left, and a gang that is not admitted gets only the rest.
func TestAdmittedGangsGoInOrder(t *testing.T) {
	mk := func(free int64, arrivalA, arrivalB int, bNeed int64) (schedPlan, []*schedObject) {
		a := obj("lab/a", "g1", arrivalA, nil, need(pods("a", "p1:D", "p2"), 100, gi)...)
		b := obj("lab/b", "g2", arrivalB, nil, need(pods("b", "p1:D", "p2"), 100, bNeed)...)
		c := obj("lab/c", "g3", 3, nil, need(pods("c", "p1"), 100, gi/2)...)
		env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, free)}, empty: []nodeRoom{room("n1", 4000, 3*gi)}}
		objs := []*schedObject{c, b, a}
		return planSchedule(objs, 10, false, env), objs
	}
	// The reviewer's probe: room for one remainder. a arrived first and goes; b and c wait.
	plan, _ := mk(gi, 1, 2, gi)
	wantDispatch(t, plan, "a/p2")
	for _, id := range []string{"lab/b", "lab/c"} {
		if r := plan.status[id].Reason; r != laboratoryv1alpha1.WaitInsufficient {
			t.Fatalf("%s reason = %q", id, r)
		}
	}
	// The order is the arrival, not the input: b first, a waits.
	plan, _ = mk(gi, 2, 1, gi)
	wantDispatch(t, plan, "b/p2")
	// b waits behind a and keeps the half Gi left for itself: c does not take it.
	plan, _ = mk(gi+gi/2, 1, 2, gi)
	wantDispatch(t, plan, "a/p2")
	// Room for both: both go, and c gets the half Gi nobody keeps.
	plan, _ = mk(2*gi+gi/2, 1, 2, gi)
	wantDispatch(t, plan, "a/p2", "b/p2", "c/p1")
	// b's remainder fits no empty node, so it keeps nothing: c backfills into the room a does not keep.
	plan, _ = mk(gi+gi/2, 1, 2, 4*gi)
	wantDispatch(t, plan, "a/p2", "c/p1")
	if r := plan.status["lab/b"].Reason; r != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("b reason = %q", r)
	}
}

// A pod over what the schedulable nodes give together, but within a cordoned node of the pool, waits: it is not failed.
func TestPodOverSchedulableRoomButWithinThePoolWaits(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1"), 100, 6*gi)...)
	small := []nodeRoom{room("n1", 4000, 2*gi), room("n2", 4000, 2*gi)}
	env := &fakeEnv{nodes: small, empty: small, maxNode: &amount{cpu: 4000, mem: 8 * gi}, fits: map[string]fit{"gang/g": fitNever}}
	env.fits["a/p1"] = fitNever
	env.neverOff = true
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if len(plan.failed) != 0 || plan.status["lab/a"].Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("failed %+v, status %+v", plan.failed, plan.status["lab/a"])
	}
}
