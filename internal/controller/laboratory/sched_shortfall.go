package laboratory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

// This file is the capacity side of the reservations (spec items 6 and 19), run by the scheduler at the end of its pass, after
// the plan and the status writes, so it never races them:
//   - the shortfall: the peak overlap of the active and future reservations placed on the empty lab nodes (the same withholding
//     as the plan: emptiest node first, ratio kept), and what the active reservations find no room for on the nodes now
//     (plan.shortfall). It goes to the CapacityShortfall condition of every tenant with an active or future reservation and to
//     the gauge laboratory_reservation_shortfall;
//   - the cleanup of unreserved work: when the active reservations find no room now because unreserved work (no reservation
//     label) holds it, every unreserved Lab and LabGroup of the tenants whose work holds that room is marked with
//     AnnotationEvictAt (now + the notice) and deleted when that time passes, after the shortfall and the object are checked
//     again past the cache. The mark goes as soon as the shortfall clears. The cluster cannot know which unreserved work
//     matters, so it does not pick: all of such a tenant's unreserved objects are marked.

var reservationShortfall = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "laboratory_reservation_shortfall",
	Help: "What the reservations need beyond what the lab nodes hold (cpu in cores, memory in bytes): the larger of the " +
		"shortfall at their peak overlap on the empty nodes and the room the active reservations find no place for now.",
}, []string{"resource"})

func init() { metrics.Registry.MustRegister(reservationShortfall) }

// usageMaxAge is how old status.usedAt may be for the Exceeded deletion of the ReservationReconciler: twice the scheduler's
// status interval, at least a minute.
func usageMaxAge(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = defaultStatusInterval
	}
	return max(2*interval, time.Minute)
}

// ReservationUsageMaxAge is usageMaxAge for the manager: the ReservationReconciler's UsageMaxAge for a scheduler with this
// status interval (zero: the default), so the heartbeat and the gate cannot diverge.
func ReservationUsageMaxAge(statusInterval time.Duration) time.Duration {
	return usageMaxAge(statusInterval)
}

// usageHeartbeat is how often the scheduler writes status.usedAt of a reservation whose usage did not change: half the
// freshness bound, so a running scheduler always keeps it fresh.
func usageHeartbeat(interval time.Duration) time.Duration { return usageMaxAge(interval) / 2 }

// demandWindow is a piece of time in which the same reservations are open, with what they reserve together.
type demandWindow struct {
	from, to time.Time
	names    []string
	sum      amount
}

// demandWindows cuts [from, to) at every start and end of the reservations that count (not being deleted, not ended, reserving
// something) and returns every piece in which at least one of them is open.
func demandWindows(res []laboratoryv1alpha1.Reservation, from, to time.Time) []demandWindow {
	type span struct {
		name     string
		amt      amount
		from, to time.Time
	}
	var spans []span
	cuts := []time.Time{from, to}
	for i := range res {
		r := &res[i]
		a := reservationAmount(r)
		if r.DeletionTimestamp != nil || r.Status.Phase == laboratoryv1alpha1.ReservationPhaseEnded || a == (amount{}) {
			continue
		}
		f, t := r.Spec.From.Time, r.Spec.To.Time
		if f.Before(from) {
			f = from
		}
		if t.After(to) {
			t = to
		}
		if !f.Before(t) {
			continue
		}
		spans = append(spans, span{name: r.Name, amt: a, from: f, to: t})
		cuts = append(cuts, f, t)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].name < spans[j].name })
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	var out []demandWindow
	for i := 0; i+1 < len(cuts); i++ {
		if !cuts[i].Before(cuts[i+1]) {
			continue
		}
		w := demandWindow{from: cuts[i], to: cuts[i+1]}
		for _, sp := range spans {
			if !sp.from.After(w.from) && sp.to.After(w.from) {
				w.names = append(w.names, sp.name)
				w.sum = w.sum.add(sp.amt)
			}
		}
		if len(w.names) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// peakDemand is the most the reservations reserve at the same time within [from, to), per resource (the CPU and the memory
// peaks may fall in different pieces of time).
func peakDemand(res []laboratoryv1alpha1.Reservation, from, to time.Time) corev1.ResourceList {
	var peak amount
	for _, w := range demandWindows(res, from, to) {
		peak = amount{cpu: max(peak.cpu, w.sum.cpu), mem: max(peak.mem, w.sum.mem)}
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(peak.cpu, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(peak.mem, resource.BinarySI),
	}
}

// shortfallReport is the capacity verdict of a pass.
type shortfallReport struct {
	// known: the verdict was made (false: the capacity is unknown in this pass, so nothing is written or changed).
	known bool
	// checked: the per-node resource check is on; without it nothing is computed and no work is evicted.
	checked bool
	// peak is the peak overlap of the active and future reservations, capacity the room for labs on the empty nodes.
	peak, capacity amount
	// future is the most of a piece of time's reservations that the empty nodes cannot hold (placed as the plan withholds),
	// now what the active reservations find no room for on the nodes in this pass, short the larger of the two per resource.
	future, now, short amount
	// tenants are the tenants with an active or future reservation.
	tenants map[string]bool
	// blockers are the tenants whose unreserved work holds room the active reservations need now.
	blockers map[string]bool
}

// shortfall makes the capacity verdict of the pass from the plan.
func (e *clusterEnv) shortfall(plan schedPlan) shortfallReport {
	rep := shortfallReport{tenants: map[string]bool{}}
	horizon := e.now
	for i := range e.snap.reservations {
		r := &e.snap.reservations[i]
		if r.DeletionTimestamp == nil && r.Status.Phase != laboratoryv1alpha1.ReservationPhaseEnded && r.Spec.To.After(e.now) {
			rep.tenants[r.Spec.Tenant] = true
			if r.Spec.To.After(horizon) {
				horizon = r.Spec.To.Time
			}
		}
	}
	if !e.s.Config.ResourceCheck {
		rep.known = true
		return rep
	}
	c := e.capacity()
	if c == nil || e.err != nil {
		return shortfallReport{}
	}
	rep.known, rep.checked = true, true
	empty := c.rooms()
	for _, n := range empty {
		rep.capacity = rep.capacity.add(amount{cpu: n.CPU, mem: n.Memory})
	}
	byName := make(map[string]*laboratoryv1alpha1.Reservation, len(e.snap.reservations))
	for i := range e.snap.reservations {
		byName[e.snap.reservations[i].Name] = &e.snap.reservations[i]
	}
	for _, w := range demandWindows(e.snap.reservations, e.now, horizon) {
		rep.peak = amount{cpu: max(rep.peak.cpu, w.sum.cpu), mem: max(rep.peak.mem, w.sum.mem)}
		usage := make(map[string]reservationUsage, len(w.names))
		for _, n := range w.names {
			r := byName[n]
			usage[n] = reservationUsage{amount: reservationAmount(r), tenant: r.Spec.Tenant, active: true}
		}
		pass := newReservationPass(usage)
		pass.withhold(empty, nil)
		s := sumOf(pass.shortfall)
		rep.future = amount{cpu: max(rep.future.cpu, s.cpu), mem: max(rep.future.mem, s.mem)}
	}
	rep.now = sumOf(plan.shortfall)
	rep.short = amount{cpu: max(rep.future.cpu, rep.now.cpu), mem: max(rep.future.mem, rep.now.mem)}
	if rep.now != (amount{}) {
		rep.blockers = e.unreservedBlockers(plan, c)
	}
	return rep
}

func sumOf(m map[string]amount) amount {
	var s amount
	for _, a := range m {
		s = s.add(a)
	}
	return s
}

// condition is the CapacityShortfall condition of a tenant with an active or future reservation.
func (r shortfallReport) condition() metav1.Condition {
	c := metav1.Condition{Type: laboratoryv1alpha1.TenantConditionCapacityShortfall, Status: metav1.ConditionFalse}
	switch {
	case !r.checked:
		c.Reason, c.Message = "NotChecked", "the resource check of the scheduler is off: the capacity is not checked"
	case r.short == (amount{}):
		c.Reason, c.Message = laboratoryv1alpha1.TenantReasonReservationsFit, "the reservations fit the lab nodes"
	default:
		c.Status, c.Reason = metav1.ConditionTrue, laboratoryv1alpha1.TenantReasonReservationsDoNotFit
		c.Message = fmt.Sprintf("the reservations need up to cpu %dm and memory %d bytes at once, the lab nodes hold cpu %dm and memory %d bytes for labs: short of cpu %dm and memory %d bytes",
			r.peak.cpu, r.peak.mem, r.capacity.cpu, r.capacity.mem, r.short.cpu, r.short.mem)
		if r.now != (amount{}) {
			c.Message += fmt.Sprintf("; the open reservations find no room on the nodes now for cpu %dm and memory %d bytes", r.now.cpu, r.now.mem)
		}
	}
	return c
}

// evictable says the object is unreserved work the cleanup may take: it carries no reservation label at all (the scheduler's
// own predicate, names.ReservationOf: an empty label is an invalid reservation, never admitted) and is not being deleted.
func evictable(o metav1.Object) bool {
	_, labelled := names.ReservationOf(o.GetLabels())
	return !labelled && o.GetDeletionTimestamp() == nil
}

// unreservedHolds is the room the bound pods of each unreserved object (by objectKey) take on each node, the held pods of its
// stopped labs too, as the capacity snapshot counts it, with the tenant of each such object.
func (e *clusterEnv) unreservedHolds() (holds map[string]map[string]amount, tenantOf map[string]string) {
	labs := map[string]*laboratoryv1alpha1.Lab{}
	for _, l := range e.snap.labs {
		labs[l.Namespace+"/"+l.Name] = l
	}
	groupOf := map[string]*laboratoryv1alpha1.LabGroup{}
	for _, g := range e.snap.groups {
		groupOf[laboratoryv1alpha1.LabGroupNamespaceOf(g)] = g
	}
	holds, tenantOf = map[string]map[string]amount{}, map[string]string{}
	add := func(p *corev1.Pod) {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || !nodecap.IsLabPod(p) {
			return
		}
		var owner client.Object
		if l := p.Labels[names.LabelLab]; l != "" {
			if lab := labs[p.Namespace+"/"+l]; lab != nil {
				owner = lab
			}
		} else if g := groupOf[p.Namespace]; g != nil {
			owner = g
		}
		if owner == nil || !evictable(owner) {
			return
		}
		k := objectKey(owner)
		if holds[k] == nil {
			holds[k] = map[string]amount{}
		}
		holds[k][p.Spec.NodeName] = holds[k][p.Spec.NodeName].add(podRequests(p))
		tenantOf[k] = names.TenantOf(owner.GetLabels())
	}
	for _, p := range e.snap.pods {
		add(p)
	}
	for i := range e.heldPods {
		add(&e.heldPods[i])
	}
	return holds, tenantOf
}

// schedKey is the objectKey of the Lab or LabGroup of a plan object; "" for one without such a handle (plan tests).
func schedKey(o *schedObject) string {
	if c, ok := o.ref.(client.Object); ok {
		return objectKey(c)
	}
	return ""
}

// unboundPod is a dispatched pod not on a node yet, with the objectKey of its object.
type unboundPod struct {
	need podNeed
	obj  string
}

// counterfactual runs steps 1 to 3 of the placement simulation of a pass again, with or without some unreserved objects.
type counterfactual struct {
	c       *capacitySnapshot
	plan    schedPlan
	holds   map[string]map[string]amount
	unbound []unboundPod
	usage   map[string]reservationUsage
}

// gangIn says every object of the gang is in the set.
func gangIn(g *gang, set map[string]bool) bool {
	for _, o := range g.objs {
		if !set[schedKey(o)] {
			return false
		}
	}
	return len(g.objs) > 0
}

// frees says removing the objects takes something off the nodes in steps 1 to 3: a bound pod, an unbound one or a remainder.
func (cf counterfactual) frees(removed map[string]bool) bool {
	for k := range removed {
		if len(cf.holds[k]) > 0 {
			return true
		}
	}
	for _, u := range cf.unbound {
		if removed[u.obj] {
			return true
		}
	}
	for _, r := range cf.plan.remainders {
		if gangIn(r.g, removed) {
			return true
		}
	}
	return false
}

// shortfallWithout is the shortfall of the active reservations without the objects removed (their bound pods, their unbound
// ones and their admitted remainders); none removed is the pass itself.
func (cf counterfactual) shortfallWithout(removed map[string]bool) amount {
	used := make(map[string]amount, len(cf.c.labUsed))
	for n, a := range cf.c.labUsed {
		used[n] = a
	}
	for k := range removed {
		for n, a := range cf.holds[k] {
			used[n] = used[n].sub(a)
		}
	}
	rooms := without(cf.c.rooms(), used)
	var needs []podNeed
	for _, u := range cf.unbound {
		if !removed[u.obj] {
			needs = append(needs, u.need)
		}
	}
	rooms = placeEach(rooms, needs)
	pass := newReservationPass(cf.usage)
	for _, r := range cf.plan.remainders {
		if gangIn(r.g, removed) {
			continue
		}
		if l, ok := fitsGang(rooms, r.needs); ok {
			rooms = l
			pass.take(r.g.reservation, r.need)
		}
	}
	pass.withhold(rooms, cf.plan.withholdOrder)
	return sumOf(pass.shortfall)
}

// unreservedBlockers are the tenants whose unreserved work holds room the active reservations find no place for now: removing
// exactly what the cleanup would take of the tenant (its targets: a LabGroup kept for a reserved or foreign lab stays) makes the
// reservations' shortfall smaller (counterfactual). A tenant whose targets free nothing never blocks.
func (e *clusterEnv) unreservedBlockers(plan schedPlan, c *capacitySnapshot) map[string]bool {
	holds, tenantOf := e.unreservedHolds()
	cf := counterfactual{c: c, plan: plan, holds: holds, usage: e.fairness().usage}
	candidates := map[string]bool{}
	for _, t := range tenantOf {
		candidates[t] = true
	}
	for _, o := range e.objs {
		for _, p := range o.pods {
			if p.state != laboratoryv1alpha1.PodStarting || e.nodeOf(p) != "" {
				continue
			}
			cf.unbound = append(cf.unbound, unboundPod{need: podNeed{Key: p.key, CPU: p.need.cpu, Memory: p.need.mem}, obj: schedKey(o)})
			if m, ok := o.ref.(metav1.Object); ok && evictable(m) {
				candidates[p.tenant] = true
			}
		}
	}
	for _, r := range plan.remainders {
		if r.g.reservation == "" && !r.g.mixedReservation {
			candidates[r.g.tenant] = true
		}
	}
	base := cf.shortfallWithout(nil)
	out := map[string]bool{}
	for t := range candidates {
		removed := evictionTargets(e.snap, map[string]bool{t: true})
		if !cf.frees(removed) {
			continue
		}
		if s := cf.shortfallWithout(removed); s.cpu < base.cpu || s.mem < base.mem {
			out[t] = true
		}
	}
	return out
}

// tenantMessageInterval is the least time between two writes of a tenant's CapacityShortfall condition that change only its
// message (the numbers).
const tenantMessageInterval = time.Minute

// writeTenants sets the CapacityShortfall condition on every tenant with an active or future reservation and removes it from
// the others, when it changed, within the per-object interval and the budget of the tick. The patch carries the conditions
// and the resourceVersion as precondition: a write of the agent (status.reserved, used) in between makes it conflict and it
// is done again on a later pass, so neither writer erases the other's fields.
func (s *Scheduler) writeTenants(ctx context.Context, snap *clusterView, sf shortfallReport, now time.Time, interval time.Duration, budget int, seen map[types.UID]bool) int {
	logger := log.FromContext(ctx).WithName("scheduler")
	tenantNames := make([]string, 0, len(snap.tenants))
	for n := range snap.tenants {
		tenantNames = append(tenantNames, n)
	}
	sort.Strings(tenantNames)
	for _, n := range tenantNames {
		t := snap.tenants[n]
		seen[t.UID] = true
		if !sf.known || t.DeletionTimestamp != nil {
			continue
		}
		next := t.DeepCopy()
		var changed bool
		wait := interval
		if sf.tenants[n] {
			c := sf.condition()
			c.ObservedGeneration, c.LastTransitionTime = t.Generation, metav1.NewTime(now)
			if old := meta.FindStatusCondition(t.Status.Conditions, c.Type); old != nil && old.Status == c.Status && old.Reason == c.Reason && old.ObservedGeneration == c.ObservedGeneration {
				// only the numbers moved (they follow every dispatch): the gauge has them now, the message less often
				wait = max(interval, tenantMessageInterval)
			}
			changed = meta.SetStatusCondition(&next.Status.Conditions, c)
		} else {
			changed = meta.RemoveStatusCondition(&next.Status.Conditions, laboratoryv1alpha1.TenantConditionCapacityShortfall)
		}
		if !changed {
			continue
		}
		if at, ok := s.lastWrite[t.UID]; ok && now.Sub(at) < wait {
			continue
		}
		if budget <= 0 {
			continue
		}
		if err := s.Status().Patch(ctx, next, client.MergeFromWithOptions(t, client.MergeFromWithOptimisticLock{})); err != nil {
			if !errors.IsNotFound(err) && !errors.IsConflict(err) {
				logger.Error(err, "write the capacity shortfall of the tenant", "tenant", n)
			}
			continue
		}
		s.lastWrite[t.UID] = now
		budget--
	}
	return budget
}

// unreservedNotice is the time between marking unreserved work and deleting it: the reservations' ending notice, at least a
// minute, so a single look never deletes anything.
func (s *Scheduler) unreservedNotice() time.Duration {
	return max(s.Config.UnreservedNotice, reservationRecheck)
}

// evictAt reads the evict-at annotation of an object: has says it is there, ok that it holds a time.
func evictAt(o metav1.Object) (at time.Time, has, ok bool) {
	v, has := o.GetAnnotations()[names.AnnotationEvictAt]
	if !has {
		return time.Time{}, false, false
	}
	at, err := time.Parse(time.RFC3339, v)
	return at, true, err == nil
}

// evictionTargets are the objects of the view the cleanup is to take, by kind/namespace/name: every unreserved Lab and
// LabGroup of a blocking tenant, stopped ones too. A LabGroup whose namespace holds a lab that is reserved, being deleted or of
// another tenant is not one (deleting it would drain that lab); its unreserved labs are.
func evictionTargets(snap *clusterView, blockers map[string]bool) map[string]bool {
	out := map[string]bool{}
	if len(blockers) == 0 {
		return out
	}
	for _, l := range snap.labs {
		if evictable(l) && blockers[names.TenantOf(l.Labels)] {
			out[objectKey(l)] = true
		}
	}
	byNamespace := map[string][]*laboratoryv1alpha1.Lab{}
	for _, l := range snap.labs {
		byNamespace[l.Namespace] = append(byNamespace[l.Namespace], l)
	}
	for _, g := range snap.groups {
		t := names.TenantOf(g.Labels)
		if !evictable(g) || !blockers[t] {
			continue
		}
		foreign := false
		for _, l := range byNamespace[laboratoryv1alpha1.LabGroupNamespaceOf(g)] {
			if !evictable(l) || names.TenantOf(l.Labels) != t {
				foreign = true
			}
		}
		if !foreign {
			out[objectKey(g)] = true
		}
	}
	return out
}

func objectKey(o client.Object) string {
	kind := "lab"
	if _, ok := o.(*laboratoryv1alpha1.LabGroup); ok {
		kind = "group"
	}
	return kind + "/" + o.GetNamespace() + "/" + o.GetName()
}

// evictRetryAfter is how long a due object whose deletion the live check refused, or that failed, waits before it is tried
// again (each try reads the whole cluster past the cache).
const evictRetryAfter = 30 * time.Second

// evictUnreserved marks the targets with the evict-at annotation, removes the mark from every other object that has it, and
// deletes the targets that are due, in one pass after checking past the cache.
//
// A mark alone never deletes: an object is due only when its mark time passed AND this process has seen it as a marked target
// on every pass since at least one full notice ago (seen: the previous pass's record; any pass without a known verdict, a
// stopped scheduler or a restart starts it again). A mark this process did not give, found with less than a full notice left,
// is given again (now + the notice), so after any gap in observation the current shortfall always gets its own full notice.
// New marks and renewals take at most the status budget per tick, clears four times that.
func (s *Scheduler) evictUnreserved(ctx context.Context, snap *clusterView, blockers map[string]bool, seen map[types.UID]time.Time, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	targets := evictionTargets(snap, blockers)
	notice := s.unreservedNotice()
	budget := s.Config.StatusBudget
	if budget <= 0 {
		budget = defaultStatusBudget
	}
	clears := 4 * budget
	objs := make([]client.Object, 0, len(snap.labs)+len(snap.groups))
	for _, l := range snap.labs {
		objs = append(objs, l)
	}
	for _, g := range snap.groups {
		objs = append(objs, g)
	}
	next := map[types.UID]time.Time{}
	var due []client.Object
	marked := 0
	mark := func(o client.Object) bool {
		if budget <= 0 {
			return false
		}
		budget--
		when := now.Add(notice).UTC().Format(time.RFC3339)
		if err := s.annotate(ctx, o, when); err != nil {
			logger.Error(err, "mark unreserved work for deletion", "object", objectKey(o))
			return false
		}
		marked++
		if s.Recorder != nil {
			s.Recorder.Eventf(o, corev1.EventTypeWarning, "UnreservedEviction",
				"active reservations need the room the unreserved work of tenant %s holds: this object is deleted at %s unless the shortfall clears", names.TenantOf(o.GetLabels()), when)
		}
		return true
	}
	for _, o := range objs {
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		uid := o.GetUID()
		want := targets[objectKey(o)]
		at, has, ok := evictAt(o)
		switch {
		case want && ok:
			first, known := seen[uid]
			if !known {
				// a mark from before a gap (or another process): this shortfall gets its own full notice
				first = now
				if at.Before(now.Add(notice)) && !mark(o) {
					continue // not renewed this pass: not seen either, so it stays undue
				}
			}
			next[uid] = first
			if !now.Before(at) && !now.Before(first.Add(notice)) && !now.Before(s.evictRetry[uid]) {
				due = append(due, o)
			}
		case want:
			if mark(o) {
				next[uid] = now
			}
		case has:
			if clears <= 0 {
				continue
			}
			clears--
			if err := s.annotate(ctx, o, ""); err != nil {
				logger.Error(err, "remove the eviction mark", "object", objectKey(o))
				continue
			}
			if s.Recorder != nil {
				s.Recorder.Event(o, corev1.EventTypeNormal, "UnreservedEvictionCancelled", "the active reservations no longer need the room of this unreserved work: it is not deleted")
			}
		}
	}
	s.evictSeen = next
	for uid := range s.evictRetry {
		if _, ok := next[uid]; !ok {
			delete(s.evictRetry, uid)
		}
	}
	if marked > 0 {
		logger.Info("unreserved work holds the room of active reservations: marked for deletion", "objects", marked, "notice", notice)
	}
	if len(due) > 0 {
		s.deleteEvicted(ctx, due, now)
	}
}

// annotate sets the evict-at annotation of an object to value, or removes it when value is empty.
func (s *Scheduler) annotate(ctx context.Context, o client.Object, value string) error {
	orig := o.DeepCopyObject().(client.Object)
	a := o.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if value == "" {
		delete(a, names.AnnotationEvictAt)
	} else {
		a[names.AnnotationEvictAt] = value
	}
	o.SetAnnotations(a)
	return client.IgnoreNotFound(s.Patch(ctx, o, client.MergeFrom(orig)))
}

// deleteEvicted deletes, in one pass, the marked objects whose time passed, once a view read past the cache says the same: a
// dry run of the plan on it still finds the active reservations short because the object's tenant holds the room, and the live
// object is the one marked (UID), still unreserved, still a target and still due. Each goes with its UID and resourceVersion
// as precondition. What is not deleted waits evictRetryAfter before the next try.
func (s *Scheduler) deleteEvicted(ctx context.Context, due []client.Object, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	if s.evictRetry == nil {
		s.evictRetry = map[types.UID]time.Time{}
	}
	// whatever is not deleted below waits before the next try
	for _, o := range due {
		s.evictRetry[o.GetUID()] = now.Add(evictRetryAfter)
	}
	live, err := s.load(ctx, true)
	if err != nil {
		logger.Error(err, "check the eviction of unreserved work")
		return
	}
	objs := s.objects(live, now)
	flight := 0
	for _, o := range objs {
		for _, p := range o.pods {
			if p.state == laboratoryv1alpha1.PodStarting {
				flight++
			}
		}
	}
	env := &clusterEnv{s: s, ctx: ctx, snap: live, now: now, objs: objs, live: true}
	plan := planSchedule(objs, s.Config.MaxPods-flight, s.Config.MaxPods <= 0, env)
	if env.err != nil {
		logger.Error(env.err, "check the eviction of unreserved work")
		return
	}
	sf := env.shortfall(plan)
	if !sf.known {
		return
	}
	targets := evictionTargets(live, sf.blockers)
	current := map[string]client.Object{}
	for _, l := range live.labs {
		current[objectKey(l)] = l
	}
	for _, g := range live.groups {
		current[objectKey(g)] = g
	}
	for _, o := range due {
		key := objectKey(o)
		cur := current[key]
		if cur == nil || cur.GetUID() != o.GetUID() || !targets[key] {
			continue
		}
		if at, _, ok := evictAt(cur); !ok || now.Before(at) {
			continue
		}
		// the version read: a label added since (the object adopted by a reservation) makes it conflict
		uid, rv := cur.GetUID(), cur.GetResourceVersion()
		if err := s.Delete(ctx, cur, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
			if errors.IsNotFound(err) || errors.IsConflict(err) {
				continue // gone, changed, or another object has the name now
			}
			logger.Error(err, "delete unreserved work", "object", key)
			continue
		}
		logger.Info("deleted unreserved work: active reservations need its room", "object", key, "tenant", names.TenantOf(cur.GetLabels()))
	}
}
