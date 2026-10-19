package laboratory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// This file is the pure core of the scheduler: given the pods of the top-level
// objects and the free slots, which pods start now, and what each waiting object
// reports. Everything that touches the cluster is in scheduler.go.

// unsetMaxWait bounds the wait for an object its reconciler has not left a trace on (Status.ImageCache) yet, from its creation:
// long enough for the reconciler's queue at the start of an event (about 150 labs), short enough that one lab whose reconcile
// keeps failing before its first trace does not hold its group for the whole event.
const unsetMaxWait = 15 * time.Minute

// oversizedGang is a gang that cannot start whole on the lab nodes, even empty, with the nodes still to come up to the maximum.
type oversizedGang struct {
	objs    []*schedObject
	message string
}

// memText writes a memory amount for people: Mi or Gi when it is a whole number of them, else bytes.
func memText(b int64) string {
	switch {
	case b != 0 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGi", b>>30)
	case b != 0 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMi", b>>20)
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMi", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", b)
}

// largestRoom is the room with the most memory, then CPU.
func largestRoom(rooms []nodeRoom) nodeRoom {
	var big nodeRoom
	for _, r := range rooms {
		if r.Memory > big.Memory || r.Memory == big.Memory && r.CPU > big.CPU {
			big = r
		}
	}
	return big
}

// unsetSettleWindow is how long after its reconciler first worked on it a gang waits for an object whose pods nobody has set
// up yet (its Devices not made, its scheduling state not recorded); after that the gang goes on without it.
const unsetSettleWindow = 2 * time.Minute

// Kinds of pod owner.
const (
	kindDevicePod = "device"
	kindGroupPod  = "group"
)

// schedPod is one pod of an object: a container Device of a Lab, or the VPN or
// gateway pod of a LabGroup.
type schedPod struct {
	// lookup is the key of the pod in the cluster view (podsOf): where the pod would be found once it exists.
	lookup string
	// key identifies the pod: "ns/lab/device" or "group/vpn".
	key  string
	name string // sort key inside the object
	kind string
	// state is the recorded scheduling state; empty until the owning reconciler
	// has initialised it (the pod is then counted but cannot be dispatched).
	state laboratoryv1alpha1.PodScheduleState
	// need is what the pod will request from a node.
	need amount
	// tenant owns the pod (its object's tenant label).
	tenant string
	// missing says the pod is a device pod whose Device does not exist (yet): the lab reconciler has not made it.
	missing bool
	// queuedAt is when the pod last entered the queue (its scheduling record; a resumed lab is queued again); zero: unknown.
	queuedAt time.Time
	// ref is the scheduler's handle on the underlying object (opaque to the plan).
	ref any
}

func (p *schedPod) dispatched() bool {
	switch p.state {
	case laboratoryv1alpha1.PodStarting, laboratoryv1alpha1.PodStarted, laboratoryv1alpha1.PodFailed:
		return true
	}
	return false
}

// done is a pod that no longer holds anything up: Ready once, or failed.
func (p *schedPod) done() bool {
	return p.state == laboratoryv1alpha1.PodStarted || p.state == laboratoryv1alpha1.PodFailed
}

func (p *schedPod) queued() bool { return p.state == laboratoryv1alpha1.PodQueued }

// schedObject is a top-level object: a LabGroup or a Lab.
type schedObject struct {
	id      string // "lab/ns/name" or "group/name"
	group   string // deploy group key; empty: independent
	after   []string
	arrival time.Time
	// priority is the explicit dispatch priority (higher first); it wins over size.
	priority int
	pods     []*schedPod
	// prepKey and images say what the nodes must have before the object's first pod
	// starts: the images of the whole deploy group (or of the independent lab).
	prepKey string
	images  []string
	// settleFrom is when the object's reconciler first worked on it, from which unsetSettleWindow counts; zero: not yet (no
	// limit on waiting for its pods to be set up).
	settleFrom time.Time
	// prepTenant is the tenant whose registry credentials pull the images (none: anonymous).
	prepTenant string
	// reservation is the Reservation the object runs under (its reservation label); empty: unreserved, unless labelled.
	reservation string
	// labelled says the object carries the reservation label; with an empty reservation the label names none, which is
	// invalid: the object's gang is refused with ReservationMissing (gangReservation).
	labelled bool
	// ref is the scheduler's handle on the Lab or LabGroup (opaque to the plan).
	ref any
}

// isGroup says the object is a LabGroup: its pods are the VPN and gateway of a team, not lab pods.
func (o *schedObject) isGroup() bool { return strings.HasPrefix(o.id, "group/") }

// groupKey identifies a deploy group inside its tenant: the same key in two tenants is two groups, for gangs and for
// deploy-after alike.
func groupKey(tenant, group string) string { return tenant + "\x00" + group }

// objTenant is the tenant of the object: that of its first pod (all pods of an object share it).
func objTenant(o *schedObject) string {
	if len(o.pods) == 0 {
		return ""
	}
	return o.pods[0].tenant
}

// size is what the object's pods request together: larger labs are dispatched first, so the big ones are placed while the
// nodes are still empty and the small ones fill the gaps.
func (o *schedObject) size() amount {
	var sum amount
	for _, p := range o.pods {
		sum = sum.add(p.need)
	}
	return sum
}

func (o *schedObject) pending() int {
	n := 0
	for _, p := range o.pods {
		if !p.dispatched() {
			n++
		}
	}
	return n
}

func (o *schedObject) started() bool {
	for _, p := range o.pods {
		if p.dispatched() {
			return true
		}
	}
	return false
}

// schedEnv is what the plan asks of the cluster: whether the images of an object are on the nodes, the room labs have on
// each node, and the tenant quotas. Room is decided only by the placement simulation on the per-node rooms.
type schedEnv interface {
	// prepared reports whether the images of the object are on the nodes; it may
	// start pulling them.
	prepared(o *schedObject) bool
	// noNodes says the resource check is on, no lab node is schedulable now and no node still to come may be placed on: the
	// pass waits.
	noNodes() bool
	// take counts a dispatched pod toward its tenant's quota for the rest of the pass.
	take(p *schedPod)
	// tenantFits says whether the pod's tenant stays within its CPU and memory quota
	// with the pod added to what its dispatched pods already request.
	tenantFits(p *schedPod) bool
	// rooms is the room labs have on each node now less the lab pods bound to it (the start of the placement simulation), the
	// virtual rooms of the nodes still to come up to the maximum after them; false: no per-node check (the resource check is off).
	rooms() ([]nodeRoom, bool)
	// nodeOf is the node a dispatched pod is bound to; empty: not on a node yet.
	nodeOf(p *schedPod) string
	// emptyRooms is the room labs have on each node with no lab running, the nodes still to come up to the maximum included,
	// for the DoesNotFit verdict of a gang; nil: no per-node verdict.
	emptyRooms() []nodeRoom
	// neverFits says the pod's request exceeds what any single node can give (allocatable less the reserves, whatever runs
	// there), so waiting will not help.
	neverFits(p *schedPod) bool
	// tenantEverFits says whether the pod's need fits the tenant's quota with nothing else running.
	tenantEverFits(p *schedPod) bool
	// fairness is the usage of the reservations, the clock and the aging threshold of the pass.
	fairness() fairness
	// stoppedHolds is the room the pods of stopped labs keep on each node (they stay until the lab is resumed or deleted);
	// nil: none.
	stoppedHolds() map[string]amount
}

// objectStatus is what the plan reports for an object that still has pods to dispatch.
type objectStatus struct {
	Position, Length int32
	Reason, Message  string
	Pods, Pending    int32
}

// failedPod is a pod the plan gave up on: it cannot fit any node.
type failedPod struct {
	pod     *schedPod
	message string
	// reason is the failure reason; empty: DoesNotFit.
	reason string
}

// schedPlan is the outcome of one scheduling pass.
type schedPlan struct {
	dispatch []*schedPod
	failed   []failedPod
	// unsettled are the objects left out of their gang because their pods were still not set up unsetSettleWindow after
	// they were created.
	unsettled []*schedObject
	// oversized are the gangs that do not fit even the empty lab nodes now (they wait with DoesNotFitNow), with the message
	// that says so.
	oversized []oversizedGang
	// status holds the queue place of every object with undispatched pods; an
	// object absent from it has nothing waiting.
	status map[string]objectStatus
	// left is the per-node room after the placement simulation of the pass: the bound and unbound lab pods, the held
	// remainders and the gangs dispatched now are off it. nil: no per-node check.
	left []nodeRoom
	// withheld is the room kept for the unused part of each active reservation (reservation -> node -> room) before the new
	// gangs of the pass were placed, and withheldLeft what of it is still kept after them (a reservation's own gangs use it).
	// Both are in left: the room is free, the others do not get it. nil: no per-node check.
	withheld, withheldLeft map[string]map[string]amount
	// shortfall is what of the unused part of an active reservation the nodes have no room for now.
	shortfall map[string]amount
	// remainders are the remainders of the admitted gangs that step 2 of the placement simulation tried, in its order (placed
	// or not), and withholdOrder the order of step 3: what sched_shortfall.go needs to run steps 1 to 3 again without a
	// tenant's unreserved work. nil: no per-node check.
	remainders    []remainder
	withholdOrder []string
}

// remainder is the rest of an admitted gang that may hold room in step 2 (its reservation and tenant admit it, it fits the
// empty nodes).
type remainder struct {
	g     *gang
	needs []podNeed
	need  amount
}

type schedGroup struct {
	key      string
	tenant   string
	objs     []*schedObject
	arrival  time.Time
	started  bool
	pending  int
	complete bool
	after    map[string]bool
}

// planSchedule decides which pods start now.
//
// The conveyor: gangs. A gang is the not yet dispatched pods of one deploy group (an independent lab and
// every LabGroup object are gangs of their own), and it starts whole or not at all: it needs a slot for every
// pod, the tenant quota for the sum of its pods, its images on the nodes, and a placement of every pod on the
// per-node rooms (fitsGang). Groups that are being dispatched (some pod out, some pending) go first, then
// groups that have not started, in arrival order, each only once every group it lists in deploy-after is
// complete; independent labs go last. A gang that waits for tenant quota, images or room does not stop the
// others (backfill); no slot, or no node at all, stops the pass, so the order is never skipped. A gang with a
// pod that fits no node, or that never fits its tenant's quota, is
// failed whole: none of it starts (a gang that only fails to pack the nodes as a whole waits with DoesNotFit and is looked at on every pass). A gang that has pods out is admitted: on every pass its remaining pods are
// placed on the nodes before any other gang is considered, in arrival order among the admitted gangs, and the room they take
// is kept for it; one that does not place holds nothing in that pass (see the placement simulation below). A gang bigger than the whole in-flight limit goes
// in slices of the free slots, its room checked as a whole. The pods of
// LabGroups (the team's VPN and gateway) go ahead of all labs.
//
// Fairness (gangOrder): after the admitted gangs, the new gangs under a reservation go first, the lowest satisfied share of
// the reservation first, and the unreserved ones after them in the conveyor order. A gang waiting longer than the aging
// threshold keeps its room against the others (see aging below), so backfill cannot starve it.
//
// Reservations: a gang that carries a reservation label is admitted only if the reservation exists for the gang's tenant (else
// ReservationMissing), its window is open (else ReservationNotActive) and what its dispatched pods, its held remainders and
// the gangs admitted under it in the pass request, with the gang added, stays within its amount (else ReservationFull; an
// invalid reservation reserves nothing). The unused part of every active reservation is withheld on the nodes (see below): no
// gang of another reservation, no unreserved gang and no aged gang places on it. Pods already out are never touched.
//
// Inside a group (and among the independent objects) the order is: an object already being dispatched first, then the
// explicit priority (higher first), then the larger object (CPU, then memory), then arrival; the order inside an
// object is kept.
//
//nolint:gocyclo // one decision over many cases; splitting it would scatter the rule
func planSchedule(objs []*schedObject, slots int, unlimited bool, env schedEnv) schedPlan {
	groups := map[string]*schedGroup{}
	var independents []*schedObject
	for _, o := range objs {
		if o.group == "" {
			independents = append(independents, o)
			continue
		}
		g := groups[groupKey(objTenant(o), o.group)]
		if g == nil {
			g = &schedGroup{key: o.group, tenant: objTenant(o), complete: true, after: map[string]bool{}, arrival: o.arrival}
			groups[groupKey(g.tenant, o.group)] = g
		}
		g.objs = append(g.objs, o)
		if o.arrival.Before(g.arrival) {
			g.arrival = o.arrival
		}
		g.started = g.started || o.started()
		g.pending += o.pending()
		for _, p := range o.pods {
			if !p.done() {
				g.complete = false
			}
		}
		for _, a := range o.after {
			g.after[a] = true
		}
	}

	// waitFor names the first group a group still depends on; empty when it may start.
	waitFor := func(g *schedGroup) (string, bool) {
		deps := make([]string, 0, len(g.after))
		for d := range g.after {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			dg := groups[groupKey(g.tenant, d)]
			if dg == nil {
				return d, false // not known (yet)
			}
			if !dg.complete || dg == g {
				return d, true
			}
		}
		return "", true
	}

	var eligible, blocked []*schedGroup
	for _, g := range groups {
		if d, _ := waitFor(g); d == "" {
			eligible = append(eligible, g)
		} else {
			blocked = append(blocked, g)
		}
	}
	byArrival := func(list []*schedGroup) {
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].arrival.Equal(list[j].arrival) {
				return list[i].arrival.Before(list[j].arrival)
			}
			if list[i].tenant != list[j].tenant {
				return list[i].tenant < list[j].tenant
			}
			return list[i].key < list[j].key
		})
	}
	byArrival(eligible)
	byArrival(blocked)
	sort.SliceStable(eligible, func(i, j int) bool {
		si, sj := eligible[i].started && eligible[i].pending > 0, eligible[j].started && eligible[j].pending > 0
		return si && !sj
	})

	orderObjs := func(list []*schedObject) []*schedObject {
		out := append([]*schedObject(nil), list...)
		sort.SliceStable(out, func(i, j int) bool {
			pi, pj := out[i].started() && out[i].pending() > 0, out[j].started() && out[j].pending() > 0
			if pi != pj {
				return pi
			}
			if out[i].priority != out[j].priority {
				return out[i].priority > out[j].priority
			}
			if si, sj := out[i].size(), out[j].size(); si != sj {
				if si.cpu != sj.cpu {
					return si.cpu > sj.cpu
				}
				return si.mem > sj.mem
			}
			if !out[i].arrival.Equal(out[j].arrival) {
				return out[i].arrival.Before(out[j].arrival)
			}
			return out[i].id < out[j].id
		})
		return out
	}
	// The unit of admission is a gang: the labs of one deploy group, an independent lab, or one LabGroup object.
	// The service pods of a LabGroup (VPN, gateway) have a lane of their own: they dispatch ahead of every
	// queued lab, so a new team is not held up by a burst of labs that wait for room. They still need a
	// slot and room like any pod; labs keep their strict order.
	var lane, rest []*gang
	add := func(key string, o ...*schedObject) {
		if len(o) == 0 {
			return
		}
		gg := &gang{key: key, tenant: objTenant(o[0]), objs: o, arrival: o[0].arrival}
		if gg.tenant != "" {
			gg.key = gg.tenant + "/" + key
		}
		for _, ob := range o {
			if ob.arrival.Before(gg.arrival) {
				gg.arrival = ob.arrival
			}
		}
		gg.reservation, gg.mixedReservation = gangReservation(o)
		if o[0].isGroup() {
			lane = append(lane, gg)
		} else {
			rest = append(rest, gg)
		}
	}
	split := func(key string, list []*schedObject) {
		var labs []*schedObject
		for _, o := range list {
			if o.isGroup() {
				add(o.id, o)
			} else {
				labs = append(labs, o)
			}
		}
		if key == "" {
			for _, o := range labs {
				add(o.id, o)
			}
			return
		}
		// A gang never spans tenants: the same deploy group key in two tenants is two gangs, so one tenant's
		// failure or quota cannot touch the other's pods.
		var order []string
		byTenant := map[string][]*schedObject{}
		for _, o := range labs {
			t := objTenant(o)
			if _, seen := byTenant[t]; !seen {
				order = append(order, t)
			}
			byTenant[t] = append(byTenant[t], o)
		}
		for _, t := range order {
			add(key, byTenant[t]...)
		}
	}
	for _, g := range eligible {
		split(g.key, orderObjs(g.objs))
	}
	split("", orderObjs(independents))
	fair := env.fairness()
	gangs := append(gangOrder(lane, fair.usage), gangOrder(rest, fair.usage)...)
	res := newReservationPass(fair.usage)
	blockedObjs := make([]*schedObject, 0, len(blocked))
	for _, g := range blocked {
		blockedObjs = append(blockedObjs, orderObjs(g.objs)...)
	}

	plan := schedPlan{status: map[string]objectStatus{}}
	gone := map[*schedPod]bool{} // dispatched or failed in this pass

	// The in-flight limit as the plan sees it: the free slots plus the pods starting now.
	limit := slots
	for _, o := range objs {
		for _, p := range o.pods {
			if p.state == laboratoryv1alpha1.PodStarting {
				limit++
			}
		}
	}

	blocker := ""
	tenantHeld := map[string]bool{}   // objects held back by their tenant's quota
	prepHeld := map[string]bool{}     // objects whose group's images are still being pulled
	roomHeld := map[string]bool{}     // objects whose gang does not fit now: the others go past them (backfill)
	resHeld := map[string][2]string{} // objects whose gang its reservation does not admit now: the Wait reason and message
	fitHeld := map[string]bool{}      // objects whose gang does not fit even the empty nodes (may change when nodes return)
	fitMsg := map[string]string{}     // the message of such an object: its gang, what it requests, the largest empty node
	unsetHeld := map[string]string{}  // objects of a gang with no pod out that waits for a pod nobody initialised yet: whose
	hold := func(held map[string]bool, gg *gang) {
		for _, o := range gg.objs {
			held[o.id] = true
		}
	}
	// An object whose pods nobody initialised yet (the reconciler has not recorded their state) belongs to its gang: a gang
	// with no pod out waits for it (it never starts without it), and the remainder of an admitted gang keeps room for it too,
	// but never longer than unsetMaxWait from its creation, and for a pod whose Device was never made (missing) only within
	// unsetSettleWindow of when its reconciler first worked on the object (settleFrom; zero: not touched yet, it is still in
	// the reconciler's queue); a pod whose Device exists but has no state yet is in the Device reconciler's queue and only the
	// cap bounds it. The earlier of the two limits that apply decides. An object still not set up after that (its
	// reconciler cannot make its Devices) is out of its gang whole, all its pods not out yet, so it neither holds the others
	// forever nor starts partly; it is reported in plan.unsettled and joins its gang as a remainder once it is set up.
	unsettled := map[*schedObject]bool{}
	out := func(o *schedObject) bool {
		if unsettled[o] {
			return true
		}
		if fair.now.IsZero() {
			return false
		}
		capped := fair.now.Sub(o.arrival) >= unsetMaxWait
		windowed := !o.settleFrom.IsZero() && fair.now.Sub(o.settleFrom) >= unsetSettleWindow
		for _, p := range o.pods {
			// A pod whose Device exists but has no scheduling state yet is in the Device reconciler's queue: only the cap
			// bounds it. One whose Device was never made is what the window after the reconciler's trace is for.
			if p.state == "" && (capped || windowed && p.missing) {
				unsettled[o] = true
				plan.unsettled = append(plan.unsettled, o)
				return true
			}
		}
		return false
	}
	queuedOf := func(gg *gang) []*schedPod {
		var queued []*schedPod
		for _, o := range gg.objs {
			if out(o) {
				continue
			}
			pods := append([]*schedPod(nil), o.pods...)
			sort.SliceStable(pods, func(i, j int) bool { return pods[i].name < pods[j].name })
			for _, p := range pods {
				if p.queued() && !gone[p] {
					queued = append(queued, p)
				}
			}
		}
		return queued
	}
	// unsetOf are the pods of the gang nobody initialised yet, of its objects that are not out of it.
	unsetOf := func(gg *gang) []*schedPod {
		var unset []*schedPod
		for _, o := range gg.objs {
			if out(o) {
				continue
			}
			for _, p := range o.pods {
				if p.state == "" && !gone[p] {
					unset = append(unset, p)
				}
			}
		}
		return unset
	}
	// unsetObject names the object of the gang that has the unset pod.
	unsetObject := func(gg *gang, unset *schedPod) string {
		for _, o := range gg.objs {
			for _, p := range o.pods {
				if p == unset {
					return o.id
				}
			}
		}
		return ""
	}
	needsOf := func(pods []*schedPod) []podNeed {
		needs := make([]podNeed, 0, len(pods))
		for _, p := range pods {
			needs = append(needs, podNeed{Key: p.key, CPU: p.need.cpu, Memory: p.need.mem})
		}
		return needs
	}

	// The room of a pass is ONE placement simulation over the nodes, in which every pod goes whole onto one node (its CPU and
	// memory from the same room):
	//  1. it starts from the room labs have on each node less every lab pod bound there (env.rooms: the real nodes, then the
	//     virtual rooms of the nodes still to come up to the static maximum), and places the pods that are dispatched but not
	//     on a node yet, first-fit decreasing: real room first, so the ones waiting for a new node hold the virtual room;
	//  2. then the remainders of the admitted gangs (a gang that has pods out), in their strict order: arrival, tenant, key.
	//     A remainder that places is held: the rooms shrink by its placement and the gang dispatches against it. One that
	//     does not place holds nothing in this pass and waits, so it never blocks the gangs after it;
	//  3. the unused part of every active reservation is withheld on what is left (reservationPass.withhold), the reservations
	//     of the gangs in their order first: it stays in the rooms, but only the reservation's own gangs are offered it;
	//  4. new gangs are placed on what is left, in the conveyor order, with backfill (try). A gang under a reservation is
	//     placed on the room withheld for it alone if it fits there, else on the free room and its own withheld room together;
	//     either way the withheld room shrinks by what the gang takes of it and to what the reservation has left.
	//
	// The remainders of admitted gangs come before the withheld room: their gang was admitted whole against the room offered to
	// it then (atomicity), and a remainder under a reservation counts against that reservation's amount.
	rooms, perNode := env.rooms()
	if perNode {
		var unbound []podNeed
		for _, o := range objs {
			for _, p := range o.pods {
				if p.state == laboratoryv1alpha1.PodStarting && env.nodeOf(p) == "" {
					unbound = append(unbound, podNeed{Key: p.key, CPU: p.need.cpu, Memory: p.need.mem})
				}
			}
		}
		rooms = placeEach(rooms, unbound)
	}
	// emptyFor is the room the gang could ever have now: the empty rooms less its own dispatched pods (they stay while its
	// lab runs), the bound ones on their nodes, the others placed as in step 1. nil: no per-node verdict.
	emptyFor := func(gg *gang) []nodeRoom {
		empty := env.emptyRooms()
		if empty == nil {
			return nil
		}
		bound := map[string]amount{}
		var unbound []podNeed
		for _, o := range gg.objs {
			for _, p := range o.pods {
				if !p.dispatched() {
					continue
				}
				if n := env.nodeOf(p); n != "" {
					bound[n] = bound[n].add(p.need)
				} else if p.state == laboratoryv1alpha1.PodStarting {
					unbound = append(unbound, podNeed{Key: p.key, CPU: p.need.cpu, Memory: p.need.mem})
				}
			}
		}
		return placeEach(without(empty, bound), unbound)
	}
	// A remainder is held only while the gang can dispatch: its tenant within the quota (counting the room held for the
	// tenant's earlier gangs), and it fits the empty nodes beside its own pods.
	held := map[*gang]bool{}                 // admitted gangs whose remainder is placed: rooms is already less it
	heldQuota := map[string]amount{}         // tenant -> the remainders held for it
	heldOwn := map[*gang]map[string]amount{} // the part of heldQuota that is the gang's own
	if perNode {
		byPriority := append([]*gang(nil), gangs...)
		sort.SliceStable(byPriority, func(i, j int) bool {
			a, b := byPriority[i], byPriority[j]
			if !a.arrival.Equal(b.arrival) {
				return a.arrival.Before(b.arrival)
			}
			if a.tenant != b.tenant {
				return a.tenant < b.tenant
			}
			return a.key < b.key
		})
		for _, gg := range byPriority {
			started := false
			for _, o := range gg.objs {
				started = started || o.started()
			}
			q := append(queuedOf(gg), unsetOf(gg)...)
			if !started || len(q) == 0 {
				continue
			}
			sums := map[string]amount{}
			var need amount
			for _, p := range q {
				sums[p.tenant] = sums[p.tenant].add(p.need)
				need = need.add(p.need)
			}
			eligible := res.gate(gg, need) == ""
			for t, sum := range sums {
				if !env.tenantFits(&schedPod{key: "gang/" + gg.key, tenant: t, need: sum.add(heldQuota[t])}) {
					eligible = false
				}
			}
			needs := needsOf(q)
			// The images of a started gang are on the nodes already (an operator restart forgets that, so they are not asked
			// again, here or in try).
			if empty := emptyFor(gg); eligible && empty != nil {
				_, eligible = fitsGang(empty, needs)
			}
			if !eligible {
				continue
			}
			plan.remainders = append(plan.remainders, remainder{g: gg, needs: needs, need: need})
			left, ok := fitsGang(rooms, needs)
			if !ok {
				continue // holds nothing: the gangs after it go on
			}
			rooms = left
			held[gg] = true
			heldOwn[gg] = sums
			for t, sum := range sums {
				heldQuota[t] = heldQuota[t].add(sum)
			}
			res.take(gg.reservation, need)
		}
		var order []string
		for _, gg := range gangs {
			if gg.reservation != "" && !gg.mixedReservation && fair.usage[gg.reservation].tenant == gg.tenant {
				order = append(order, gg.reservation)
			}
		}
		res.withhold(rooms, order)
		plan.withheld, plan.shortfall, plan.withholdOrder = res.snapshot(), res.shortfall, order
	}

	// Aging: a gang that has waited (since it last entered the queue, waitingSince) longer than the aging threshold is aged.
	// The oldest aged gang that could dispatch but for the room (its tenant within the quota, its images on the nodes, it packs
	// the empty nodes beside its own pods and the pods stopped labs keep) keeps its placement on those rooms: the other gangs place only on the room not kept for it, so a stream of
	// small gangs cannot keep taking the room it waits for. One gang at a time, placed by the same simulation; the held
	// remainders of the admitted gangs were placed before it, so it never holds them back. Only lab gangs age and only lab
	// gangs are held back: the LabGroup lane (the VPN and gateway of a team) is placed before it and uses all the room, so a new
	// team always gets its network. A group still waiting for its deploy-after groups is not a gang yet, so it never ages
	// into the reserver. The aged gang is offered the room right after the lane and the admitted gangs, and the room is free
	// for the others again once it dispatches or turns out not eligible. Reservations come before it: it is eligible only within
	// its own reservation (if any), it packs beside the room withheld for the other reservations, it is placed on the free room
	// and its own withheld room only, and what it keeps is taken from the free room the others see, never from their withheld
	// room.
	var aged *gang
	var agedRoom map[string]amount // the room kept for the aged gang; nil: none
	agedPlacement := func(gg *gang) (map[string]amount, bool) {
		q := queuedOf(gg)
		unset := unsetOf(gg)
		if len(q) == 0 || (!gg.started() && len(unset) > 0) {
			return nil, false
		}
		q = append(q, unset...)
		sums := map[string]amount{}
		var need amount
		for _, p := range q {
			if env.neverFits(p) {
				return nil, false
			}
			sums[p.tenant] = sums[p.tenant].add(p.need)
			need = need.add(p.need)
		}
		if res.gate(gg, need) != "" {
			return nil, false
		}
		for t, sum := range sums {
			sp := &schedPod{key: "gang/" + gg.key, tenant: t, need: sum}
			if !env.tenantEverFits(sp) {
				return nil, false
			}
			sp.need = sum.add(heldQuota[t])
			if !env.tenantFits(sp) {
				return nil, false
			}
		}
		if !gg.started() {
			for _, o := range gg.objs {
				if !env.prepared(o) {
					return nil, false
				}
			}
		}
		empty := emptyFor(gg)
		if empty == nil {
			return nil, false
		}
		// A stopped lab keeps its room for days: room kept beside it would stay closed to everyone while the gang cannot start.
		empty = without(empty, env.stoppedHolds())
		// The room withheld for other reservations is not its to keep: it stays closed to it for as long as they are unused.
		empty = without(empty, res.total(gg.reservation))
		needs := needsOf(q)
		left, ok := fitsGang(empty, needs)
		if !ok {
			return nil, false
		}
		return placed(empty, left), true
	}
	if perNode && fair.aging > 0 && !env.noNodes() {
		var candidates []*gang
		for _, gg := range gangs {
			if !held[gg] && !gg.objs[0].isGroup() && fair.now.Sub(gg.waitingSince()) >= fair.aging {
				candidates = append(candidates, gg)
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if wa, wb := a.waitingSince(), b.waitingSince(); !wa.Equal(wb) {
				return wa.Before(wb)
			}
			return byAge(a, b)
		})
		for _, gg := range candidates {
			if room, ok := agedPlacement(gg); ok {
				aged, agedRoom = gg, room
				break
			}
		}
	}
	order := gangs
	if aged != nil {
		at := 0
		for i, gg := range gangs {
			if (gg.started() || gg.objs[0].isGroup()) && gg != aged {
				at = i + 1
			}
		}
		order = make([]*gang, 0, len(gangs))
		for i, gg := range gangs {
			if i == at {
				order = append(order, aged)
			}
			if gg != aged {
				order = append(order, gg)
			}
		}
		if at == len(gangs) {
			order = append(order, aged)
		}
	}
	var sequence []*schedObject
	for _, gg := range order {
		sequence = append(sequence, gg.objs...)
	}

	failAll := func(pods []*schedPod, reason, message string) {
		for _, p := range pods {
			plan.failed = append(plan.failed, failedPod{pod: p, message: message, reason: reason})
			gone[p] = true
		}
	}
	// try admits the gang whole or not at all. It reports a stop of the pass: no slots, or no node to place on.
	try := func(gg *gang) (stop bool) {
		queued := queuedOf(gg)
		// A gang with no pod out waits whole until every pod of it is initialised, so it never starts without some of its
		// pods; the remainder of an admitted gang sends what is queued and keeps the room and quota of the rest.
		unset := unsetOf(gg)
		if len(unset) > 0 && !gg.started() {
			who := unsetObject(gg, unset[0])
			for _, o := range gg.objs {
				unsetHeld[o.id] = who
			}
			return false
		}
		if len(queued) == 0 {
			return false
		}
		if gg == aged {
			defer func() {
				// The room kept for it is free for the others again once it dispatched or turned out not eligible; it stays
				// while the gang waits only for room, or when the pass stops at it.
				if !stop && !roomHeld[gg.objs[0].id] {
					agedRoom = nil
				}
			}()
		}
		// One pod that no single node can ever hold fails the whole gang: nothing of it starts, since its other pods
		// would only burn room. Anything else is decided by the placement on the per-node rooms: a gang that does not pack
		// even the empty nodes (beside its own running pods) waits with DoesNotFitNow and is looked at on every pass, since a
		// cordoned or rebooting node of the pool may return (neverFits counts it).
		var total amount
		for _, p := range queued {
			if env.neverFits(p) {
				failAll(queued, laboratoryv1alpha1.FailureDoesNotFit, "the pod "+p.key+" requests more than any single node can give")
				return false
			}
			total = total.add(p.need)
		}
		all := append(append([]*schedPod(nil), queued...), unset...)
		for _, p := range unset {
			total = total.add(p.need)
		}
		if total != (amount{}) && env.noNodes() {
			blocker = laboratoryv1alpha1.WaitNoSchedulableNodes
			return true
		}
		needs := needsOf(all)
		gg.needs = needs
		notFit, oversize := false, ""
		if empty := emptyFor(gg); empty != nil {
			if _, ok := fitsGang(empty, needs); !ok {
				notFit = true
				var sum amount
				for _, n := range needs {
					sum = sum.add(amount{cpu: n.CPU, mem: n.Memory})
				}
				big := largestRoom(empty)
				what := "the deploy group " + gg.objs[0].group
				if gg.objs[0].group == "" {
					what = "the lab " + strings.TrimPrefix(gg.objs[0].id, "lab/")
					if gg.objs[0].isGroup() {
						what = "the lab group " + strings.TrimPrefix(gg.objs[0].id, "group/")
					}
				}
				oversize = fmt.Sprintf("%s requests cpu %dm and memory %s in all and cannot start whole on the current lab nodes: "+
					"the largest empty one has room for cpu %dm and memory %s", what, sum.cpu, memText(sum.mem), big.CPU, memText(big.Memory))
			}
		}
		// The quota of the tenant is checked on the sum of the gang's needs.
		sums := map[string]*schedPod{}
		var tenants []string
		for _, p := range all {
			sp := sums[p.tenant]
			if sp == nil {
				sp = &schedPod{key: "gang/" + gg.key, tenant: p.tenant}
				sums[p.tenant] = sp
				tenants = append(tenants, p.tenant)
			}
			sp.need = sp.need.add(p.need)
		}
		sort.Strings(tenants)
		if gg.tenant == "" {
			gg.tenant = tenants[0]
		}
		for _, t := range tenants {
			if !env.tenantEverFits(sums[t]) {
				failAll(queued, laboratoryv1alpha1.FailureTenantQuota, "the group never fits the quota of the tenant "+t)
				return false
			}
		}
		// The images of a group are pulled before its first pod starts, also while the gang waits for quota or room. A started
		// gang's images are on the nodes already: they are not asked again (an operator restart forgets the done pulls).
		started, ready := false, true
		for _, o := range gg.objs {
			started = started || o.started()
		}
		if !started {
			for _, o := range gg.objs {
				ready = env.prepared(o) && ready
			}
		}
		// A tenant at its quota waits without holding up the others: the gang is skipped.
		for _, t := range tenants {
			// The room kept for the tenant's other admitted gangs counts against its quota.
			other := heldQuota[t].sub(heldOwn[gg][t])
			if !env.tenantFits(&schedPod{key: sums[t].key, tenant: t, need: sums[t].need.add(other)}) {
				hold(tenantHeld, gg)
				return false
			}
		}
		// A held remainder was admitted by its reservation when it was placed.
		if why := res.gate(gg, total); why != "" && !held[gg] {
			msg := ""
			switch {
			case gg.emptyReservation():
				msg = "the reservation label is empty: it names no reservation"
			case gg.mixedReservation:
				msg = "objects of the group belong to different reservations"
			case why == laboratoryv1alpha1.WaitReservationMissing:
				msg = "the reservation " + gg.reservation + " does not exist for the tenant"
			case why == laboratoryv1alpha1.WaitReservationNotActive:
				msg = "the window of the reservation " + gg.reservation + " is not open now"
			default:
				msg = "the group does not fit what is left of the reservation " + gg.reservation
			}
			for _, o := range gg.objs {
				resHeld[o.id] = [2]string{why, msg}
			}
			return false
		}
		// The images gate only the gang's own objects: the others go on, and only the order and dependency rules hold them.
		if !ready {
			hold(prepHeld, gg)
			return false
		}
		if notFit {
			hold(fitHeld, gg)
			for _, o := range gg.objs {
				fitMsg[o.id] = oversize
			}
			plan.oversized = append(plan.oversized, oversizedGang{objs: gg.objs, message: oversize})
			return false
		}
		// A held remainder dispatches against its own placement; any other gang is placed on what is left.
		left := rooms
		var at map[string]amount // where the gang is placed now; nil: held already, or no per-node check
		if perNode && !held[gg] {
			// Every lab gang but the aged one places only on the room not kept for the aged gang; the lane is never held back.
			// No gang places on the room withheld for another reservation; a gang under a reservation takes its own first.
			var keep map[string]amount
			if agedRoom != nil && gg != aged && !gg.objs[0].isGroup() {
				keep = agedRoom
			}
			if gg.reservation != "" {
				mine := res.own(rooms, gg.reservation)
				if l, ok := fitsGang(mine, needs); ok {
					at = placed(mine, l)
				}
			}
			if at == nil {
				free := res.view(rooms, gg.reservation, keep)
				l, ok := fitsGang(free, needs)
				if !ok {
					hold(roomHeld, gg)
					return false
				}
				at = placed(free, l)
			}
			left = without(rooms, at)
		}
		// A gang within the limit needs a slot for every pod. One larger than the whole limit could never get that: it
		// goes in slices of the free slots, while the room is still checked for all of it.
		send := queued
		if !unlimited {
			if slots < len(queued) && (len(queued) <= limit || slots <= 0) {
				blocker = laboratoryv1alpha1.WaitInFlightLimit
				return true
			}
			send = queued[:min(len(queued), slots)]
		}
		rooms = left
		if !held[gg] {
			// The whole gang counts against its reservation, also when it goes in slices.
			res.use(gg.reservation, at, total)
		}
		if held[gg] {
			// What it sends is in the tenant's dispatched pods now (take): it no longer counts as held.
			for _, p := range send {
				heldQuota[p.tenant] = heldQuota[p.tenant].sub(p.need)
				heldOwn[gg][p.tenant] = heldOwn[gg][p.tenant].sub(p.need)
			}
		}
		for _, p := range send {
			env.take(p)
			plan.dispatch = append(plan.dispatch, p)
			gone[p] = true
		}
		slots -= len(send)
		return false
	}

	for _, gg := range order {
		if try(gg) {
			break
		}
	}
	if perNode {
		plan.left = rooms
		plan.withheldLeft = res.snapshot()
	}

	// Queue places: eligible objects with pods still waiting, in conveyor order,
	// then the objects of groups that wait for another group.
	remaining := func(o *schedObject) int {
		n := 0
		for _, p := range o.pods {
			if !p.dispatched() && !gone[p] {
				n++
			}
		}
		return n
	}
	var waiting []*schedObject
	for _, o := range append(append([]*schedObject(nil), sequence...), blockedObjs...) {
		if remaining(o) > 0 {
			waiting = append(waiting, o)
		}
	}
	for i, o := range waiting {
		st := objectStatus{
			Position: int32(i + 1), Length: int32(len(waiting)),
			Pods: int32(len(o.pods)), Pending: int32(remaining(o)),
		}
		if g := groups[groupKey(objTenant(o), o.group)]; g != nil && o.group != "" {
			if dep, known := waitFor(g); dep != "" {
				st.Reason = laboratoryv1alpha1.WaitForGroup
				st.Message = "waiting for group " + dep
				if !known {
					st.Message += " (not known yet)"
				}
				plan.status[o.id] = st
				continue
			}
		}
		if unsettled[o] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitForTurn, "the pods of this object are still not set up: its group goes on without it"
			plan.status[o.id] = st
			continue
		}
		if tenantHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitTenantQuota, "the tenant has reached its quota"
			plan.status[o.id] = st
			continue
		}
		if why, ok := resHeld[o.id]; ok {
			st.Reason, st.Message = why[0], why[1]
			plan.status[o.id] = st
			continue
		}
		if prepHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitPreparingImages, "pulling the images of the group onto the nodes"
			plan.status[o.id] = st
			continue
		}
		if who, ok := unsetHeld[o.id]; ok {
			st.Reason, st.Message = laboratoryv1alpha1.WaitForTurn, "waiting for the pods of "+who+" to be set up"
			plan.status[o.id] = st
			continue
		}
		if fitHeld[o.id] && !roomHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitDoesNotFit, fitMsg[o.id]
			plan.status[o.id] = st
			continue
		}
		if roomHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitInsufficient, "not enough free resources for all pods of the group now"
			plan.status[o.id] = st
			continue
		}
		st.Reason = blocker
		if st.Reason == "" {
			st.Reason = laboratoryv1alpha1.WaitForTurn
		}
		plan.status[o.id] = st
	}
	return plan
}
