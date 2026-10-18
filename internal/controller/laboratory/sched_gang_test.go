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

func TestGangTooBigForEmptyLabIsFailed(t *testing.T) {
	o := obj("lab/a", "g", 1, nil, need(pods("a", "p1", "p2"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 4*gi)}, fits: map[string]fit{"gang/g": fitNever}}
	plan := planSchedule([]*schedObject{o}, 10, false, env)
	wantDispatch(t, plan)
	if len(plan.failed) != 2 {
		t.Fatalf("failed = %d", len(plan.failed))
	}
}
