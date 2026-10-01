package laboratory

import (
	"sort"
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
	// key identifies the pod: "ns/lab/device" or "group/vpn".
	key  string
	name string // sort key inside the object
	kind string
	// state is the recorded scheduling state; empty until the owning reconciler
	// has initialised it (the pod is then counted but cannot be dispatched).
	state laboratoryv1alpha1.PodScheduleState
	// retryAt is set on a queued pod whose retry was requested: it goes first.
	retryAt time.Time
	// need is what the pod will request from a node.
	need amount
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
	pods    []*schedPod
	// prepKey and images say what the nodes must have before the object's first pod
	// starts: the images of the whole deploy group (or of the independent lab).
	prepKey string
	images  []string
	// ref is the scheduler's handle on the Lab or LabGroup (opaque to the plan).
	ref any
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
	objs     []*schedObject
	arrival  time.Time
	started  bool
	pending  int
	complete bool
	after    map[string]bool
}

// planSchedule decides which pods start now.
//
// Retried pods go first, whatever else is queued. Then the conveyor: groups that
// are being dispatched (some pod out, some pending) keep the slots, then groups
// that have not started, in arrival order, each only once every group it lists in
// deploy-after is complete; objects without a group go last. Inside a group an
// object is dispatched completely before the next one starts, and an object may
// start with fewer slots than pods. Dispatch stops at the first pod that cannot go
// (no slot, images not ready, no room), so the order is never skipped; a pod that
// fits no node at all is failed instead, and a group that is not eligible is
// skipped without stopping the others.
func planSchedule(objs []*schedObject, slots int, unlimited bool, env schedEnv) schedPlan {
	groups := map[string]*schedGroup{}
	var independents []*schedObject
	for _, o := range objs {
		if o.group == "" {
			independents = append(independents, o)
			continue
		}
		g := groups[o.group]
		if g == nil {
			g = &schedGroup{key: o.group, complete: true, after: map[string]bool{}, arrival: o.arrival}
			groups[o.group] = g
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
			dg := groups[d]
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
		sort.Slice(list, func(i, j int) bool {
			if !list[i].arrival.Equal(list[j].arrival) {
				return list[i].arrival.Before(list[j].arrival)
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
			if !out[i].arrival.Equal(out[j].arrival) {
				return out[i].arrival.Before(out[j].arrival)
			}
			return out[i].id < out[j].id
		})
		return out
	}
	var sequence []*schedObject
	for _, g := range eligible {
		sequence = append(sequence, orderObjs(g.objs)...)
	}
	sequence = append(sequence, orderObjs(independents)...)
	var blockedObjs []*schedObject
	for _, g := range blocked {
		blockedObjs = append(blockedObjs, orderObjs(g.objs)...)
	}

	plan := schedPlan{status: map[string]objectStatus{}}
	gone := map[*schedPod]bool{} // dispatched or failed in this pass

	// Retried pods first, oldest request first.
	var heads []*schedPod
	for _, o := range objs {
		for _, p := range o.pods {
			if p.queued() && !p.retryAt.IsZero() {
				heads = append(heads, p)
			}
		}
	}
	sort.SliceStable(heads, func(i, j int) bool {
		if !heads[i].retryAt.Equal(heads[j].retryAt) {
			return heads[i].retryAt.Before(heads[j].retryAt)
		}
		return heads[i].key < heads[j].key
	})

	blocker := ""
	try := func(p *schedPod, o *schedObject, withImages bool) (stop bool) {
		if !p.queued() || gone[p] {
			return false
		}
		if !unlimited && slots <= 0 {
			blocker = laboratoryv1alpha1.WaitInFlightLimit
			return true
		}
		if withImages && !env.prepared(o) {
			blocker = laboratoryv1alpha1.WaitPreparingImages
			return true
		}
		switch env.check(p) {
		case fitWait:
			blocker = laboratoryv1alpha1.WaitInsufficient
			return true
		case fitNoNodes:
			blocker = laboratoryv1alpha1.WaitNoSchedulableNodes
			return true
		case fitNever:
			plan.failed = append(plan.failed, failedPod{pod: p, message: "the pod requests more than every node can give"})
			gone[p] = true
			return false
		}
		env.take(p)
		plan.dispatch = append(plan.dispatch, p)
		gone[p] = true
		slots--
		return false
	}

	owner := map[*schedPod]*schedObject{}
	for _, o := range objs {
		for _, p := range o.pods {
			owner[p] = o
		}
	}
	stopped := false
	for _, p := range heads {
		if try(p, owner[p], false) {
			stopped = true
			break
		}
	}
	if !stopped {
	conveyor:
		for _, o := range sequence {
			pods := append([]*schedPod(nil), o.pods...)
			sort.SliceStable(pods, func(i, j int) bool { return pods[i].name < pods[j].name })
			for _, p := range pods {
				if try(p, o, true) {
					break conveyor
				}
			}
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
		if g := groups[o.group]; g != nil && o.group != "" {
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
		st.Reason = blocker
		if st.Reason == "" {
			st.Reason = laboratoryv1alpha1.WaitForTurn
		}
		plan.status[o.id] = st
	}
	return plan
}
