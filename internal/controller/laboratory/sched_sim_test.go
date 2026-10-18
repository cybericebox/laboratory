package laboratory

import (
	"fmt"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

// The remainder of an admitted gang is placed whole: a pod that only node x can hold takes CPU and memory from x, so a new
// gang of the same size does not also count on x.
func TestAdmittedRemainderIsPlacedWholeInBothDimensions(t *testing.T) {
	nodes := []nodeRoom{room("x", 4000, 2*gi), room("y", 1000, 4*gi)}
	a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1:D", "p2"), 2000, 2*gi)...)
	c := obj("lab/c", "", 2, nil, need(pods("c", "p1"), 2000, 2*gi)...)
	plan := planSchedule([]*schedObject{c, a}, 10, false, &fakeEnv{nodes: nodes, empty: nodes})
	wantDispatch(t, plan, "a/p2")
	if r := plan.status["lab/c"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("c reason = %q", r)
	}
	if !packs(nodes, plan.dispatch) {
		t.Fatalf("the dispatched pods do not fit the nodes: %v", keys(plan.dispatch))
	}
}

// A pod that is dispatched but not on a node yet is placed whole too: it does not take memory from one node and CPU from
// another.
func TestUnboundPodIsPlacedWhole(t *testing.T) {
	nodes := []nodeRoom{room("x", 4000, 2*gi), room("y", 1000, 4*gi)}
	u := obj("lab/u", "", 1, nil, need(pods("u", "p1:S"), 2000, 2*gi)...)
	c := obj("lab/c", "", 2, nil, need(pods("c", "p1"), 2000, 2*gi)...)
	env := &fakeEnv{nodes: nodes, empty: nodes, podNode: map[string]string{"u/p1": ""}}
	plan := planSchedule([]*schedObject{u, c}, 10, false, env)
	wantDispatch(t, plan)
	// bound already, the pod is in the rooms: c goes
	env.podNode = nil
	wantDispatch(t, planSchedule([]*schedObject{u, c}, 10, false, env), "c/p1")
}

// An admitted gang whose remainder cannot fit beside its own running pod holds nothing: another lab takes the free room.
func TestAdmittedGangThatCannotFitBesideItsPodsHoldsNothing(t *testing.T) {
	mk := func(podNode map[string]string) schedPlan {
		a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1:D", "p2"), 100, 2*gi)...)
		c := obj("lab/c", "g2", 2, nil, need(pods("c", "p1"), 100, gi/2)...)
		env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}, empty: []nodeRoom{room("n1", 4000, 3*gi)}, podNode: podNode}
		return planSchedule([]*schedObject{a, c}, 10, false, env)
	}
	// a's pod runs on n1: 3Gi less its 2Gi can never hold the other 2Gi.
	plan := mk(map[string]string{"a/p1": "n1"})
	wantDispatch(t, plan, "c/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("a reason = %q", r)
	}
	// its pod elsewhere: the empty node would hold it, so it only waits for room, and still holds nothing now
	plan = mk(nil)
	wantDispatch(t, plan, "c/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("a reason = %q", r)
	}
}

// Of two admitted gangs, the first that does not place does not block the next one that does.
func TestAdmittedGangThatDoesNotPlaceDoesNotBlockTheNext(t *testing.T) {
	a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1:D"), 100, gi)...)
	a.pods = append(a.pods, need(pods("a", "p2"), 100, gi+gi/2)...)
	b := obj("lab/b", "g2", 2, nil, need(pods("b", "p1:D", "p2"), 100, gi)...)
	env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, gi)}, empty: []nodeRoom{room("n1", 4000, 3*gi)},
		podNode: map[string]string{"a/p1": "n1", "b/p1": "n1"}}
	plan := planSchedule([]*schedObject{a, b}, 10, false, env)
	wantDispatch(t, plan, "b/p2")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("a reason = %q", r)
	}
}

// simEnv is a cluster for the property test: nodes with their empty room and the pods bound to them.
type simEnv struct {
	empty []nodeRoom
	node  map[string]string // dispatched pod key -> node; "": not on a node yet
	use   map[string]amount // node -> the requests of the pods bound to it
	fair  fairness
}

func (e *simEnv) prepared(*schedObject) bool      { return true }
func (e *simEnv) noNodes() bool                   { return false }
func (e *simEnv) take(*schedPod)                  {}
func (e *simEnv) tenantFits(*schedPod) bool       { return true }
func (e *simEnv) tenantEverFits(*schedPod) bool   { return true }
func (e *simEnv) emptyRooms() []nodeRoom          { return append([]nodeRoom(nil), e.empty...) }
func (e *simEnv) nodeOf(p *schedPod) string       { return e.node[p.key] }
func (e *simEnv) neverFits(p *schedPod) bool      { return !packs(e.empty, []*schedPod{p}) }
func (e *simEnv) rooms() ([]nodeRoom, bool)       { return without(e.empty, e.use), true }
func (e *simEnv) stoppedHolds() map[string]amount { return nil }
func (e *simEnv) fairness() fairness              { return e.fair }
func (e *simEnv) String() string                  { return fmt.Sprintf("%+v", e.empty) }

// packs says whether the pods can go onto the rooms, each whole onto one, by an exhaustive search (not fitsGang).
func packs(rooms []nodeRoom, ps []*schedPod) bool {
	left := append([]nodeRoom(nil), rooms...)
	items := make([]amount, 0, len(ps))
	for _, p := range ps {
		if p.need != (amount{}) {
			items = append(items, p.need)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mem > items[j].mem })
	var try func(i int) bool
	try = func(i int) bool {
		if i == len(items) {
			return true
		}
		for n := range left {
			if items[i].cpu <= left[n].CPU && items[i].mem <= left[n].Memory {
				left[n].CPU -= items[i].cpu
				left[n].Memory -= items[i].mem
				ok := try(i + 1)
				left[n].CPU += items[i].cpu
				left[n].Memory += items[i].mem
				if ok {
					return true
				}
			}
		}
		return false
	}
	return try(0)
}

// splitmix is a fixed-seed generator: the scenarios are the same on every run.
type splitmix uint64

func (s *splitmix) next(n int) int {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % uint64(n))
}

// For every generated scenario, the pods dispatched in a pass, the pods dispatched earlier but not on a node yet and the
// bound pods together fit the nodes, every pod whole on one node; a gang starts whole or not at all; and no gang holds room
// in a pass where it does not dispatch (here nothing but room can hold a gang, so a placed remainder always dispatches): the
// room the pass takes off the nodes is at most what the dispatched and the unbound pods request. Every other seed runs with
// aging on (the gangs that arrived in the first two seconds are aged): the room kept for an aged gang is never taken.
func TestPlacementSimulationNeverOverAdmits(t *testing.T) {
	const mi = int64(1) << 20
	for seed := 1; seed <= 400; seed++ {
		r := splitmix(seed)
		env := &simEnv{node: map[string]string{}, use: map[string]amount{}}
		if seed%2 == 0 {
			env.fair = fairness{now: planEpoch.Add(3 * time.Second), aging: 2 * time.Second}
		}
		for i, n := 0, 1+r.next(3); i < n; i++ {
			env.empty = append(env.empty, room(fmt.Sprintf("n%d", i), int64(1000+500*r.next(8)), int64(1+r.next(8))*512*mi))
		}
		// what the generator has placed on each node: the bound pods and the unbound ones on the node they will get
		used := append([]nodeRoom(nil), env.empty...)
		var objs []*schedObject
		var unbound []*schedPod
		for g, n := 0, 1+r.next(5); g < n; g++ {
			id := fmt.Sprintf("g%d", g)
			o := obj("lab/"+id, id, r.next(5), nil)
			for i, k := 0, 1+r.next(3); i < k; i++ {
				p := pods(id, fmt.Sprintf("p%d", i))[0]
				p.need = amount{cpu: int64(100 * (1 + r.next(30))), mem: int64(1+r.next(8)) * 256 * mi}
				o.pods = append(o.pods, p)
			}
			// a started gang: its first pods are out, on a node that had room for them
			if r.next(2) == 0 {
				for _, p := range o.pods[:1+r.next(len(o.pods))] {
					on := -1
					for _, i := range []int{r.next(len(used)), 0, 1, 2} {
						if i < len(used) && p.need.cpu <= used[i].CPU && p.need.mem <= used[i].Memory {
							on = i
							break
						}
					}
					if on < 0 {
						break
					}
					used[on].CPU -= p.need.cpu
					used[on].Memory -= p.need.mem
					p.state = laboratoryv1alpha1.PodStarted
					if r.next(3) == 0 {
						p.state = laboratoryv1alpha1.PodStarting
						env.node[p.key] = ""
						unbound = append(unbound, p)
						continue
					}
					env.node[p.key] = used[on].Name
					env.use[used[on].Name] = env.use[used[on].Name].add(p.need)
				}
			}
			objs = append(objs, o)
		}
		queued := map[*schedObject]int{}
		for _, o := range objs {
			for _, p := range o.pods {
				if p.queued() {
					queued[o]++
				}
			}
		}
		plan := planSchedule(objs, 100, false, env)
		if len(plan.failed) > 0 {
			continue // a pod over every node: nothing of its gang starts
		}
		if !packs(without(env.empty, env.use), append(append([]*schedPod(nil), unbound...), plan.dispatch...)) {
			t.Fatalf("seed %d: nodes %v, bound %v, unbound %v, dispatched %v do not fit", seed, env, env.use, keys(unbound), keys(plan.dispatch))
		}
		var taken, owed amount
		for i, r := range without(env.empty, env.use) {
			taken = taken.add(amount{cpu: r.CPU - plan.left[i].CPU, mem: r.Memory - plan.left[i].Memory})
		}
		for _, p := range append(append([]*schedPod(nil), unbound...), plan.dispatch...) {
			owed = owed.add(p.need)
		}
		if taken.cpu > owed.cpu || taken.mem > owed.mem {
			t.Fatalf("seed %d: the pass took %+v of room, the dispatched and unbound pods request %+v: a gang holds room it does not use", seed, taken, owed)
		}
		sent := map[string]int{}
		for _, p := range plan.dispatch {
			sent[p.key[:2]]++
		}
		for _, o := range objs {
			if n := sent[o.group]; n != 0 && n != queued[o] {
				t.Fatalf("seed %d: %s dispatched %d of %d pods", seed, o.id, n, queued[o])
			}
		}
	}
}

// envFromSnapshot answers the plan from a real capacitySnapshot, as clusterEnv does, without a cluster.
type envFromSnapshot struct {
	simEnv
	c capacitySnapshot
}

func (e *envFromSnapshot) rooms() ([]nodeRoom, bool) { return e.c.freeRooms(), true }
func (e *envFromSnapshot) emptyRooms() []nodeRoom    { return e.c.rooms() }
func (e *envFromSnapshot) neverFits(p *schedPod) bool {
	return p.need != (amount{}) && e.c.nodes > 0 && !e.c.fitsOneNode(p.need)
}
func (e *envFromSnapshot) noNodes() bool { return e.c.nodes == 0 }

// The reviewer's N1 probe: n1 is loaded past its platform band by a non-lab pod, so the cluster sum with the percent off
// says "wait" while n2 holds the remainder. The per-node placement is the only room check: the held remainder dispatches
// instead of keeping n2's room on every pass while nothing starts.
func TestHeldRemainderIsNotHeldBackByTheClusterSum(t *testing.T) {
	nodes := []corev1.Node{testNode("n1", "16", "16Gi"), testNode("n2", "16", "16Gi")}
	labPod := testPod("n2", "100m", "2Gi")
	labPod.Labels = map[string]string{names.LabelLab: "a", names.LabelDevice: "p1"}
	c := snapshotCapacity(nodes, []corev1.Pod{testPod("n1", "1", "15Gi"), labPod}, nil, nil, amount{}, 10, nodecap.PackingReserve{})
	env := &envFromSnapshot{simEnv: simEnv{node: map[string]string{"a/p1": "n2"}}, c: c}
	a := obj("lab/a", "g1", 1, nil, need(pods("a", "p1:D"), 100, 2*gi)...)
	a.pods = append(a.pods, need(pods("a", "p2"), 100, 12*gi)...)
	cl := obj("lab/c", "g2", 2, nil, need(pods("c", "p1"), 100, gi)...)
	plan := planSchedule([]*schedObject{a, cl}, 10, false, env)
	wantDispatch(t, plan, "a/p2")
	if _, waiting := plan.status["lab/a"]; waiting {
		t.Fatalf("a still waits: %+v", plan.status["lab/a"])
	}
	if r := plan.status["lab/c"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("c reason = %q", r)
	}
	if !packs(c.freeRooms(), plan.dispatch) {
		t.Fatalf("the dispatched pods do not fit the nodes: %v", keys(plan.dispatch))
	}
	// With a gone, c alone takes the room.
	plan = planSchedule([]*schedObject{cl}, 10, false, env)
	wantDispatch(t, plan, "c/p1")
}

// agingOn is the fairness of a pass ten minutes after planEpoch with the default aging of two minutes: a lab that arrived at
// planEpoch is aged, one that arrived a few seconds ago is not.
var agingOn = fairness{now: planEpoch.Add(10 * time.Minute), aging: 2 * time.Minute}

// fresh builds a lab of one queued pod that arrived just now (not aged).
func fresh(id string, mem int64) *schedObject {
	o := obj("lab/"+id, "", 0, nil, need(pods(id, "p1"), 100, mem)...)
	o.arrival = agingOn.now.Add(-time.Second)
	return o
}

// A big lab waits while small ones keep arriving: without aging each small lab takes the room a finished one leaves, so the
// big one never sees its 3Gi. Aged, it keeps its room: the small labs wait, and the big lab starts once the running labs are
// gone.
func TestAgedGangIsNotStarvedBySmallGangs(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	big := obj("lab/big", "", 0, nil, need(pods("big", "p1", "p2", "p3"), 100, gi)...)
	pass := func(running int64, fair fairness, objs ...*schedObject) schedPlan {
		env := &fakeEnv{nodes: []nodeRoom{room("n1", 4000, 4*gi-running*gi)}, empty: empty, fair: fair}
		return planSchedule(objs, 10, false, env)
	}
	// aging off: a small lab takes the 1Gi left by a finished one
	wantDispatch(t, pass(3, fairness{now: agingOn.now}, big, fresh("s1", gi)), "s1/p1")
	// aged: the room is kept for the big lab while the running ones end
	for running := int64(3); running >= 2; running-- {
		plan := pass(running, agingOn, big, fresh("s1", gi))
		wantDispatch(t, plan)
		for _, id := range []string{"lab/big", "lab/s1"} {
			if r := plan.status[id].Reason; r != laboratoryv1alpha1.WaitInsufficient {
				t.Fatalf("running %d: %s reason = %q", running, id, r)
			}
		}
	}
	// 3Gi free: the big lab starts, and the small one takes what it leaves
	wantDispatch(t, pass(1, agingOn, big, fresh("s1", gi)), "big/p1", "big/p2", "big/p3")
	wantDispatch(t, pass(0, agingOn, big, fresh("s1", gi)), "big/p1", "big/p2", "big/p3", "s1/p1")
}

// Only the oldest aged gang keeps room: a younger aged gang waits like any other, so the room it would need stays usable.
func TestOnlyOldestAgedGangReserves(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi), room("n2", 4000, 4*gi)}
	nodes := []nodeRoom{room("n1", 4000, 2*gi), room("n2", 4000, 2*gi)}
	oldest := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, 3*gi)...)
	younger := obj("lab/b", "", 1, nil, need(pods("b", "p1"), 100, 3*gi)...)
	c := fresh("c", 2*gi)
	// a keeps 3Gi of n1 (n1 has 2Gi free: nothing for others); b keeps nothing, so c takes n2
	plan := planSchedule([]*schedObject{younger, oldest, c}, 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn})
	wantDispatch(t, plan, "c/p1")
	for _, id := range []string{"lab/a", "lab/b"} {
		if r := plan.status[id].Reason; r != laboratoryv1alpha1.WaitInsufficient {
			t.Fatalf("%s reason = %q", id, r)
		}
	}
	// the oldest aged gang is offered the room first and frees what it kept once it dispatches: a takes n1, then b (the larger
	// lab first in the conveyor) n2, and c waits
	wide := []nodeRoom{room("n1", 4000, 3*gi), room("n2", 4000, 3*gi)}
	plan = planSchedule([]*schedObject{c, younger, oldest}, 10, false, &fakeEnv{nodes: wide, empty: empty, fair: agingOn})
	wantDispatch(t, plan, "a/p1", "b/p1")
}

// The room kept for an aged gang is free again as soon as the gang is not eligible (tenant quota, images, it does not fit
// the empty nodes) or once it dispatches; the next aged gang that is eligible keeps room instead.
func TestAgedReservationReleasedWhenIneligible(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	mk := func(mem int64) (*schedObject, *schedObject) {
		a := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, mem)...)
		a.pods[0].tenant, a.prepKey = "t1", "img-a"
		return a, fresh("c", gi)
	}
	nodes := []nodeRoom{room("n1", 4000, gi)}
	// eligible: a keeps 3Gi, c waits
	a, c := mk(3 * gi)
	wantDispatch(t, planSchedule([]*schedObject{a, c}, 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn}))
	for name, tc := range map[string]struct {
		env  *fakeEnv
		mem  int64
		want string
	}{
		"tenant quota":   {env: &fakeEnv{tenantFull: map[string]bool{"t1": true}}, mem: 3 * gi, want: laboratoryv1alpha1.WaitTenantQuota},
		"images":         {env: &fakeEnv{notPrepared: map[string]bool{"img-a": true}}, mem: 3 * gi, want: laboratoryv1alpha1.WaitPreparingImages},
		"does not fit":   {env: &fakeEnv{}, mem: 5 * gi, want: laboratoryv1alpha1.WaitDoesNotFit},
		"dispatches now": {env: &fakeEnv{}, mem: 3 * gi},
	} {
		t.Run(name, func(t *testing.T) {
			a, c := mk(tc.mem)
			tc.env.nodes, tc.env.empty, tc.env.fair = nodes, empty, agingOn
			want := []string{"c/p1"}
			if tc.want == "" {
				tc.env.nodes = []nodeRoom{room("n1", 4000, 4*gi)}
				want = []string{"a/p1", "c/p1"}
			}
			plan := planSchedule([]*schedObject{a, c}, 10, false, tc.env)
			wantDispatch(t, plan, want...)
			if r := plan.status["lab/a"].Reason; r != tc.want {
				t.Fatalf("a reason = %q, want %q", r, tc.want)
			}
		})
	}
	// a not eligible: the next aged gang b keeps its room instead, so c still waits
	a, c = mk(3 * gi)
	b := obj("lab/b", "", 1, nil, need(pods("b", "p1"), 100, 3*gi)...)
	env := &fakeEnv{nodes: nodes, empty: empty, fair: agingOn, tenantFull: map[string]bool{"t1": true}}
	wantDispatch(t, planSchedule([]*schedObject{a, b, c}, 10, false, env))
}

// assign binds pods to the rooms, each whole onto one, by an exhaustive search; nil when they do not fit.
func assign(rooms []nodeRoom, ps []*schedPod) map[string]string {
	left := append([]nodeRoom(nil), rooms...)
	order := append([]*schedPod(nil), ps...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].need.mem > order[j].need.mem })
	out := map[string]string{}
	var try func(i int) bool
	try = func(i int) bool {
		if i == len(order) {
			return true
		}
		p := order[i]
		for n := range left {
			if p.need.cpu <= left[n].CPU && p.need.mem <= left[n].Memory {
				left[n].CPU -= p.need.cpu
				left[n].Memory -= p.need.mem
				out[p.key] = left[n].Name
				if try(i + 1) {
					return true
				}
				left[n].CPU += p.need.cpu
				left[n].Memory += p.need.mem
			}
		}
		return false
	}
	if !try(0) {
		return nil
	}
	return out
}

// assignExact binds pods so that each node gets exactly the given room; nil when no binding does.
func assignExact(room map[string]amount, ps []*schedPod) map[string]string {
	left := map[string]amount{}
	var names []string
	for n, a := range room {
		left[n] = a
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]string{}
	var try func(i int) bool
	try = func(i int) bool {
		if i == len(ps) {
			for _, a := range left {
				if a != (amount{}) {
					return false
				}
			}
			return true
		}
		p := ps[i]
		for _, n := range names {
			if p.need.cpu <= left[n].cpu && p.need.mem <= left[n].mem {
				left[n] = left[n].sub(p.need)
				out[p.key] = n
				if try(i + 1) {
					return true
				}
				left[n] = left[n].add(p.need)
			}
		}
		return false
	}
	if !try(0) {
		return nil
	}
	return out
}

// Over many passes, with small labs arriving on every pass and running a few passes each, an aged big lab is never starved:
// it starts at the latest one pass after every lab that ran when it aged has ended (nothing new may take its room), and no
// pass ever dispatches more than the nodes hold. Every third seed adds what may still take the kept room: LabGroups arriving
// in the first two passes (the lane is never held back) and admitted groups with a remainder (placed before the aged gang).
// Both are finite, so the bound grows by the life of each of them, not forever.
func TestAgedGangDispatchesOnceItsRoomFrees(t *testing.T) {
	const (
		mi      = int64(1) << 20
		maxLife = 5
	)
	waited, extraWaited := 0, 0 // scenarios where the big lab could not start at once
	for seed := 1; seed <= 300; seed++ {
		r := splitmix(seed)
		extras := seed%3 == 0
		env := &simEnv{node: map[string]string{}, use: map[string]amount{}}
		for i, n := 0, 1+r.next(3); i < n; i++ {
			env.empty = append(env.empty, room(fmt.Sprintf("n%d", i), int64(2000+500*r.next(6)), int64(2+r.next(6))*512*mi))
		}
		big := obj("lab/big", "big", 0, nil)
		for i, k := 0, 1+r.next(3); i < k; i++ {
			p := pods("big", fmt.Sprintf("p%d", i))[0]
			p.need = amount{cpu: int64(100 * (1 + r.next(10))), mem: int64(1+r.next(4)) * 256 * mi}
			big.pods = append(big.pods, p)
		}
		if !packs(env.empty, big.pods) {
			continue
		}
		life := map[*schedObject]int{}
		var running, queued []*schedObject
		small := 0
		newSmall := func(arrival time.Time, n int) *schedObject {
			small++
			id := fmt.Sprintf("s%d", small)
			o := obj("lab/"+id, id, 0, nil)
			o.arrival = arrival
			for i := 0; i < n; i++ {
				p := pods(id, fmt.Sprintf("p%d", i))[0]
				p.need = amount{cpu: int64(100 * (1 + r.next(5))), mem: int64(1+r.next(3)) * 256 * mi}
				o.pods = append(o.pods, p)
			}
			return o
		}
		bind := func(p *schedPod, node string) {
			p.state = laboratoryv1alpha1.PodStarted
			env.node[p.key] = node
			env.use[node] = env.use[node].add(p.need)
		}
		// the cluster is busy when the big lab ages: small labs fill the nodes; with extras some of them are groups with only
		// their first pod out (admitted, a remainder waiting)
		remainders := 0
		for i := 0; i < 12; i++ {
			partial := extras && i%4 == 0
			o := newSmall(planEpoch, 1)
			if partial {
				o = newSmall(planEpoch.Add(9*time.Minute), 2)
			}
			a := assign(without(env.empty, env.use), o.pods[:1])
			if a == nil {
				continue
			}
			bind(o.pods[0], a[o.pods[0].key])
			// a lab ends at its time, a group with a remainder too (it may never fit beside its own pod)
			life[o] = 1 + r.next(maxLife)
			if partial {
				queued = append(queued, o)
				remainders++
				continue
			}
			running = append(running, o)
		}
		// the bound: the life of what ran when the big lab aged, plus one life per lane wave and per remainder that may take
		// the kept room before it
		limit := maxLife
		if extras {
			limit = maxLife * (2 + remainders)
		}
		teams := 0
		started := -1
		for pass := 0; pass <= limit && started < 0; pass++ {
			now := planEpoch.Add(10*time.Minute + time.Duration(pass)*2*time.Second)
			env.fair = fairness{now: now, aging: 2 * time.Minute}
			for i, k := 0, 1+r.next(3); i < k; i++ {
				queued = append(queued, newSmall(now.Add(-time.Second), 1))
			}
			if extras && pass < 2 {
				teams++
				g := obj(fmt.Sprintf("group/t%d", teams), "", 0, nil, pods(fmt.Sprintf("group/t%d", teams), "vpn")...)
				g.arrival = now.Add(-time.Second)
				g.pods[0].need = amount{cpu: 100, mem: 128 * mi}
				queued = append(queued, g)
			}
			objs := append(append([]*schedObject{big}, running...), queued...)
			plan := planSchedule(objs, 100, false, env)
			if len(plan.failed) > 0 {
				t.Fatalf("seed %d pass %d: failed %v", seed, pass, plan.failed)
			}
			free := without(env.empty, env.use)
			if assign(free, plan.dispatch) == nil {
				t.Fatalf("seed %d pass %d: nodes %v, bound %v, dispatched %v do not fit", seed, pass, env, env.use, keys(plan.dispatch))
			}
			// the pods are bound where the plan placed them (the room it took off each node), as the lab scheduler would
			at := assignExact(placed(free, plan.left), plan.dispatch)
			if at == nil {
				t.Fatalf("seed %d pass %d: the room taken %v is not the dispatched pods %v", seed, pass, placed(free, plan.left), keys(plan.dispatch))
			}
			for _, p := range plan.dispatch {
				bind(p, at[p.key])
			}
			bigOut := 0
			for _, p := range big.pods {
				if p.state == laboratoryv1alpha1.PodStarted {
					bigOut++
				}
			}
			if bigOut != 0 && bigOut != len(big.pods) {
				t.Fatalf("seed %d pass %d: the big lab started in part", seed, pass)
			}
			if bigOut != 0 {
				started = pass
			}
			// the labs that end now leave the nodes, those that start now run for their life
			end := func(o *schedObject) bool {
				if life[o]--; life[o] > 0 {
					return false
				}
				for _, p := range o.pods {
					if p.state == laboratoryv1alpha1.PodStarted {
						env.use[env.node[p.key]] = env.use[env.node[p.key]].sub(p.need)
					}
				}
				return true
			}
			var alive, still []*schedObject
			for _, o := range running {
				if !end(o) {
					alive = append(alive, o)
				}
			}
			for _, o := range queued {
				switch {
				case o.pending() == 0:
					if life[o] <= 0 {
						life[o] = 1 + r.next(maxLife)
					}
					alive = append(alive, o)
				case life[o] > 0 && end(o): // a group with a remainder ends at its time with the rest
				default:
					still = append(still, o)
				}
			}
			running, queued = alive, still
		}
		if started < 0 {
			t.Fatalf("seed %d: the aged big lab %v did not start within %d passes on nodes %v", seed, keys(big.pods), limit+1, env)
		}
		if started > 0 {
			waited++
			if extras {
				extraWaited++
			}
		}
	}
	if waited < 100 || extraWaited < 30 {
		t.Fatalf("only %d scenarios (%d with lane and remainders) made the big lab wait", waited, extraWaited)
	}
}

// The LabGroup lane (a team's VPN and gateway) is placed before the room kept for an aged lab and is never held back by it,
// so a new team always gets its network; an old LabGroup never keeps room itself. Labs are held back as before.
func TestAgedReservationNeverBlocksGroupLane(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	nodes := []nodeRoom{room("n1", 4000, gi)}
	a := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, 3*gi)...)
	team := obj("group/team", "", 0, nil, need(pods("group/team", "vpn"), 100, gi/2)...)
	team.arrival = agingOn.now.Add(-time.Second)
	plan := planSchedule([]*schedObject{a, team, fresh("c", gi/2)}, 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn})
	wantDispatch(t, plan, "group/team/vpn")
	if r := plan.status["lab/c"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("c reason = %q", r)
	}
	// an aged LabGroup that does not fit keeps nothing: the lab behind it takes the room
	old := obj("group/old", "", 0, nil, need(pods("group/old", "vpn"), 100, 3*gi)...)
	plan = planSchedule([]*schedObject{old, fresh("c", gi)}, 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn})
	wantDispatch(t, plan, "c/p1")
}

// A group still waiting for its deploy-after groups is never the aged gang that keeps room, however old; once those groups
// are complete it ages by its arrival like any other.
func TestBlockedGangNeverAgesIntoTheReserver(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	nodes := []nodeRoom{room("n1", 4000, gi)}
	mk := func(firstState string) []*schedObject {
		first := obj("lab/f", "g1", 0, nil, need(pods("f", "p1:"+firstState), 100, gi)...)
		late := obj("lab/l", "g2", 0, []string{"g1"}, need(pods("l", "p1"), 100, 3*gi)...)
		return []*schedObject{first, late, fresh("c", gi)}
	}
	// g1 still starting: l waits for it and keeps nothing, c takes the room
	plan := planSchedule(mk("S"), 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn})
	wantDispatch(t, plan, "c/p1")
	if r := plan.status["lab/l"].Reason; r != laboratoryv1alpha1.WaitForGroup {
		t.Fatalf("l reason = %q", r)
	}
	// g1 complete: l is aged and keeps its room, c waits
	plan = planSchedule(mk("D"), 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn})
	wantDispatch(t, plan)
	if r := plan.status["lab/c"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("c reason = %q", r)
	}
}

// The reviewer's probe: 8Gi empty, 2Gi of it kept by a stopped lab (6Gi free). A 7Gi aged gang fits only beside the stopped
// lab's room, which may stay for days, so it keeps nothing and the small labs run. The same 2Gi used by running labs (they
// end) still makes the gang keep its room.
func TestAgedGangDoesNotReserveBesideStoppedLabHolds(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 8*gi)}
	nodes := []nodeRoom{room("n1", 4000, 6*gi)}
	objs := func() []*schedObject {
		a := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, 7*gi)...)
		return []*schedObject{a, fresh("c", gi), fresh("d", 2*gi)}
	}
	env := &fakeEnv{nodes: nodes, empty: empty, fair: agingOn, stoppedHeld: map[string]amount{"n1": {cpu: 100, mem: 2 * gi}}}
	plan := planSchedule(objs(), 10, false, env)
	wantDispatch(t, plan, "d/p1", "c/p1")
	if r := plan.status["lab/a"].Reason; r != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("a reason = %q", r)
	}
	if left := plan.left[0].Memory; left != 3*gi {
		t.Fatalf("left %d, want 3Gi", left)
	}
	// running labs instead of the stopped one: the room is kept
	wantDispatch(t, planSchedule(objs(), 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn}))
}

// Aging counts from when the gang entered the queue, not from when its objects were created: a lab created long ago and
// resumed now (its pods queued again) is not aged on its first pass and keeps no room.
func TestResumedLabIsNotAgedOnItsFirstPass(t *testing.T) {
	empty := []nodeRoom{room("n1", 4000, 4*gi)}
	nodes := []nodeRoom{room("n1", 4000, gi)}
	mk := func(queued time.Duration) []*schedObject {
		a := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, 3*gi)...)
		a.pods[0].queuedAt = agingOn.now.Add(-queued)
		return []*schedObject{a, fresh("c", gi)}
	}
	// resumed a second ago: c takes the room
	wantDispatch(t, planSchedule(mk(time.Second), 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn}), "c/p1")
	// queued for five minutes: aged, the room is kept and c waits
	wantDispatch(t, planSchedule(mk(5*time.Minute), 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn}))
	// no queue time recorded: the arrival counts
	a := obj("lab/a", "", 0, nil, need(pods("a", "p1"), 100, 3*gi)...)
	wantDispatch(t, planSchedule([]*schedObject{a, fresh("c", gi)}, 10, false, &fakeEnv{nodes: nodes, empty: empty, fair: agingOn}))
}
