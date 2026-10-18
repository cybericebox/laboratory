package laboratory

import (
	"sort"
	"strings"
	"time"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// This file is the pure core of the scheduler: given the pods of the top-level
// objects and the free slots, which pods start now, and what each waiting object
// reports. Everything that touches the cluster is in scheduler.go.

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
	// prepTenant is the tenant whose registry credentials pull the images (none: anonymous).
	prepTenant string
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

// schedEnv is what the plan asks of the cluster: whether the images of an object
// are on the nodes, and whether a node has room for a pod.
type schedEnv interface {
	// prepared reports whether the images of the object are on the nodes; it may
	// start pulling them.
	prepared(o *schedObject) bool
	// check says whether the pod fits now, and take reserves its requests.
	check(p *schedPod) fit
	take(p *schedPod)
	// tenantFits says whether the pod's tenant stays within its CPU and memory quota
	// with the pod added to what its dispatched pods already request.
	tenantFits(p *schedPod) bool
	// rooms is the room labs have on each node now, for the per-node fit of a gang; false: no per-node check
	// (the resource check is off).
	rooms() ([]nodeRoom, bool)
	// emptyRooms is the room labs have on each node with no lab running, for the DoesNotFit verdict of a gang; nil: no
	// per-node verdict.
	emptyRooms() []nodeRoom
	// neverFits says the pod's request exceeds what any single node can give (allocatable less the reserves, whatever runs
	// there), so waiting will not help.
	neverFits(p *schedPod) bool
	// tenantEverFits says whether the pod's need fits the tenant's quota with nothing else running.
	tenantEverFits(p *schedPod) bool
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
	// status holds the queue place of every object with undispatched pods; an
	// object absent from it has nothing waiting.
	status map[string]objectStatus
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
// failed whole: none of it starts (a gang that only fails to pack the nodes as a whole waits with DoesNotFit and is looked at on every pass). A gang that has pods out is admitted: the room of its remaining pods is kept
// for it on every pass before any other gang is considered; admitted gangs go in arrival order, each behind the ones
// before it only. A gang bigger than the whole in-flight limit goes
// in slices of the free slots, its room checked as a whole. The pods of
// LabGroups (the team's VPN and gateway) go ahead of all labs.
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
	gangs := append(lane, rest...)
	var sequence []*schedObject
	for _, gg := range gangs {
		sequence = append(sequence, gg.objs...)
	}
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
	tenantHeld := map[string]bool{} // objects held back by their tenant's quota
	prepHeld := map[string]bool{}   // objects whose group's images are still being pulled
	roomHeld := map[string]bool{}   // objects whose gang does not fit now: the others go past them (backfill)
	fitHeld := map[string]bool{}    // objects whose gang does not fit even the empty nodes (may change when nodes return)
	hold := func(held map[string]bool, gg *gang) {
		for _, o := range gg.objs {
			held[o.id] = true
		}
	}
	queuedOf := func(gg *gang) []*schedPod {
		var queued []*schedPod
		for _, o := range gg.objs {
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
	needsOf := func(pods []*schedPod) ([]podNeed, []amount) {
		needs := make([]podNeed, 0, len(pods))
		amounts := make([]amount, 0, len(pods))
		for _, p := range pods {
			needs = append(needs, podNeed{Key: p.key, CPU: p.need.cpu, Memory: p.need.mem})
			amounts = append(amounts, p.need)
		}
		return needs, amounts
	}

	// A gang that has pods out is admitted: the room of the pods still to dispatch (slices of a large gang, the rest of a
	// partly started one) is kept for it on every pass, before any other gang is considered. Rooms is what is left for
	// the gangs that are not admitted yet.
	rooms, perNode := env.rooms()
	// Room is kept only while the gang can dispatch: its tenant within the quota (counting the room kept for the
	// tenant's other gangs) and the remainder fits the empty nodes. Otherwise it is released for the others.
	// Admitted gangs have a strict order among themselves: arrival, then tenant, then key (all of them are started). Each
	// competes only with the remainders of the admitted gangs before it, so two of them never block each other when the
	// room holds one but not both: the first goes, the next waits behind it and keeps what is left.
	var admitted []*gang
	rank := map[*gang]int{} // position in admitted
	remainders := map[*gang][]amount{}
	heldQuota := map[string]amount{}         // tenant -> the remainders kept for it
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
			q := queuedOf(gg)
			if !started || len(q) == 0 {
				continue
			}
			sums := map[string]amount{}
			for _, p := range q {
				sums[p.tenant] = sums[p.tenant].add(p.need)
			}
			eligible := true
			for t, sum := range sums {
				if !env.tenantFits(&schedPod{key: "gang/" + gg.key, tenant: t, need: sum.add(heldQuota[t])}) {
					eligible = false
				}
			}
			// The images of a started gang are on the nodes already (an operator restart forgets that, so they are not asked
			// again here). A remainder that does not fit even the empty nodes holds nothing: it cannot be placed, and
			// its room would be lost to everyone else.
			if empty := env.emptyRooms(); eligible && empty != nil {
				needs, amounts := needsOf(q)
				var total amount
				for _, a := range amounts {
					total = total.add(a)
				}
				_, packs := fitsGang(empty, needs)
				eligible = packs && env.check(&schedPod{key: "gang/" + gg.key, need: total}) != fitNever
			}
			if !eligible {
				continue
			}
			_, remainders[gg] = needsOf(q)
			rank[gg] = len(admitted)
			admitted = append(admitted, gg)
			heldOwn[gg] = sums
			for t, sum := range sums {
				heldQuota[t] = heldQuota[t].add(sum)
			}
		}
	}
	baseRooms := rooms
	for _, ag := range admitted {
		rooms = subtractFromEmptiest(rooms, remainders[ag])
	}
	var spent []amount       // what the gangs dispatched in this pass take
	sent := map[*gang]bool{} // admitted gangs dispatched in this pass: their room is in spent now
	// roomsBefore is the room an admitted gang may take: the rooms less the remainders of the admitted gangs before it that
	// still wait, and less what this pass dispatched.
	roomsBefore := func(gg *gang) []nodeRoom {
		out := baseRooms
		for _, ag := range admitted[:rank[gg]] {
			if !sent[ag] {
				out = subtractFromEmptiest(out, remainders[ag])
			}
		}
		return subtractFromEmptiest(out, spent)
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
		if len(queued) == 0 {
			return false
		}
		// One pod that no single node can ever hold fails the whole gang: nothing of it starts, since its other pods
		// would only burn room. A pod over the room of the schedulable nodes together only waits: a cordoned or rebooting
		// node of the pool may hold it once it returns (neverFits counts it).
		wait, notFit := false, false
		for _, p := range queued {
			if env.neverFits(p) {
				failAll(queued, laboratoryv1alpha1.FailureDoesNotFit, "the pod "+p.key+" requests more than any single node can give")
				return false
			}
			switch env.check(p) {
			case fitWait:
				wait = true
			case fitNoNodes:
				blocker = laboratoryv1alpha1.WaitNoSchedulableNodes
				return true
			case fitNever:
				notFit = true
			}
		}
		needs, _ := needsOf(queued)
		gg.needs = needs
		var total amount
		for _, p := range queued {
			total = total.add(p.need)
		}
		// A gang that does not fit the nodes as a whole may fit later (a node returns, a rollout ends): it is kept and
		// looked at on every pass, with the reason DoesNotFit. Only a single pod bigger than every node fails for good.
		switch env.check(&schedPod{key: "gang/" + gg.key, tenant: gg.tenant, need: total}) {
		case fitWait:
			wait = true
		case fitNoNodes:
			blocker = laboratoryv1alpha1.WaitNoSchedulableNodes
			return true
		case fitNever:
			notFit = true
		}
		if empty := env.emptyRooms(); empty != nil {
			if _, ok := fitsGang(empty, needs); !ok {
				notFit = true
			}
		}
		// The quota of the tenant is checked on the sum of the gang's needs.
		sums := map[string]*schedPod{}
		var tenants []string
		for _, p := range queued {
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
		// The images of a group are pulled before its first pod starts, also while the gang waits for quota or room.
		ready := true
		for _, o := range gg.objs {
			ready = env.prepared(o) && ready
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
		// The images gate only the gang's own objects: the others go on, and only the order and dependency rules hold them.
		if !ready {
			hold(prepHeld, gg)
			return false
		}
		if notFit {
			hold(fitHeld, gg)
			return false
		}
		if wait {
			hold(roomHeld, gg)
			return false
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
		if perNode {
			if _, isAdmitted := remainders[gg]; isAdmitted {
				// Its own room is already kept: it competes only with the admitted gangs before it.
				if _, ok := fitsGang(roomsBefore(gg), needs); !ok {
					hold(roomHeld, gg)
					return false
				}
				sent[gg] = true
				spent = append(spent, remainders[gg]...)
			} else {
				left, ok := fitsGang(rooms, needs)
				if !ok {
					hold(roomHeld, gg)
					return false
				}
				rooms = left
				_, amounts := needsOf(queued)
				spent = append(spent, amounts...)
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

	for _, gg := range gangs {
		if try(gg) {
			break
		}
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
		if tenantHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitTenantQuota, "the tenant has reached its quota"
			plan.status[o.id] = st
			continue
		}
		if prepHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitPreparingImages, "pulling the images of the group onto the nodes"
			plan.status[o.id] = st
			continue
		}
		if fitHeld[o.id] && !roomHeld[o.id] {
			st.Reason, st.Message = laboratoryv1alpha1.WaitDoesNotFit, "the group does not fit even on empty nodes"
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
