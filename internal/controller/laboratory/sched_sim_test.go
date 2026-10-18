package laboratory

import (
	"fmt"
	"sort"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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
}

func (e *simEnv) prepared(*schedObject) bool    { return true }
func (e *simEnv) check(*schedPod) fit           { return fitOK }
func (e *simEnv) take(*schedPod)                {}
func (e *simEnv) tenantFits(*schedPod) bool     { return true }
func (e *simEnv) tenantEverFits(*schedPod) bool { return true }
func (e *simEnv) emptyRooms() []nodeRoom        { return append([]nodeRoom(nil), e.empty...) }
func (e *simEnv) nodeOf(p *schedPod) string     { return e.node[p.key] }
func (e *simEnv) neverFits(p *schedPod) bool    { return !packs(e.empty, []*schedPod{p}) }
func (e *simEnv) rooms() ([]nodeRoom, bool)     { return without(e.empty, e.use), true }
func (e *simEnv) String() string                { return fmt.Sprintf("%+v", e.empty) }

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
// bound pods together fit the nodes, every pod whole on one node; and a gang starts whole or not at all.
func TestPlacementSimulationNeverOverAdmits(t *testing.T) {
	const mi = int64(1) << 20
	for seed := 1; seed <= 400; seed++ {
		r := splitmix(seed)
		env := &simEnv{node: map[string]string{}, use: map[string]amount{}}
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
