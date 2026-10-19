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
	for _, w := range demandWindows(e.snap.reservations, e.now, horizon) {
		rep.peak = amount{cpu: max(rep.peak.cpu, w.sum.cpu), mem: max(rep.peak.mem, w.sum.mem)}
		usage := map[string]reservationUsage{}
		for i := range e.snap.reservations {
			r := &e.snap.reservations[i]
			for _, n := range w.names {
				if r.Name == n {
					usage[n] = reservationUsage{amount: reservationAmount(r), tenant: r.Spec.Tenant, active: true}
				}
			}
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

// evictable says the object is unreserved work the cleanup may take: it carries no reservation label at all (an empty label
// counts as one) and is not being deleted.
func evictable(o metav1.Object) bool {
	_, labelled := o.GetLabels()[names.LabelReservation]
	return !labelled && o.GetDeletionTimestamp() == nil
}

// unreservedHolds is the room the bound pods of each tenant's unreserved objects take on each node (those of its stopped labs
// too), as the capacity snapshot counts it.
func (e *clusterEnv) unreservedHolds() map[string]map[string]amount {
	labs := map[string]*laboratoryv1alpha1.Lab{}
	for _, l := range e.snap.labs {
		labs[l.Namespace+"/"+l.Name] = l
	}
	groupOf := map[string]*laboratoryv1alpha1.LabGroup{}
	for _, g := range e.snap.groups {
		groupOf[laboratoryv1alpha1.LabGroupNamespaceOf(g)] = g
	}
	holds := map[string]map[string]amount{}
	add := func(p *corev1.Pod) {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || !nodecap.IsLabPod(p) {
			return
		}
		var owner metav1.Object
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
		t := names.TenantOf(owner.GetLabels())
		if holds[t] == nil {
			holds[t] = map[string]amount{}
		}
		holds[t][p.Spec.NodeName] = holds[t][p.Spec.NodeName].add(podRequests(p))
	}
	for _, p := range e.snap.pods {
		add(p)
	}
	held, _, _ := e.s.stoppedReservations(e.snap)
	for i := range held {
		add(&held[i])
	}
	return holds
}

func objEvictable(o *schedObject) bool {
	m, ok := o.ref.(metav1.Object)
	return ok && evictable(m)
}

// gangEvictable: the gang is unreserved work (no reservation, every object without the label).
func gangEvictable(g *gang) bool {
	if g.reservation != "" || g.mixedReservation {
		return false
	}
	for _, o := range g.objs {
		if !objEvictable(o) {
			return false
		}
	}
	return true
}

// unboundPod is a dispatched pod not on a node yet; tenant is the tenant whose unreserved work it is (empty: not such work).
type unboundPod struct {
	need   podNeed
	tenant string
}

// counterfactual runs steps 1 to 3 of the placement simulation of a pass again, with or without a tenant's unreserved work.
type counterfactual struct {
	c       *capacitySnapshot
	plan    schedPlan
	holds   map[string]map[string]amount
	unbound []unboundPod
	usage   map[string]reservationUsage
}

// shortfallWithout is the shortfall of the active reservations without the unreserved work of the tenant except (its bound
// pods, its unbound ones and its admitted remainders); "" keeps all of it, which is the pass itself.
func (cf counterfactual) shortfallWithout(except string) amount {
	used := make(map[string]amount, len(cf.c.labUsed))
	for n, a := range cf.c.labUsed {
		used[n] = a.sub(cf.holds[except][n])
	}
	rooms := without(cf.c.rooms(), used)
	var needs []podNeed
	for _, u := range cf.unbound {
		if except == "" || u.tenant != except {
			needs = append(needs, u.need)
		}
	}
	rooms = placeEach(rooms, needs)
	pass := newReservationPass(cf.usage)
	for _, r := range cf.plan.remainders {
		if except != "" && r.g.tenant == except && gangEvictable(r.g) {
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

// unreservedBlockers are the tenants whose unreserved work holds room the active reservations find no place for now: the
// tenant blocks when the reservations' shortfall gets smaller without its unreserved work (counterfactual).
func (e *clusterEnv) unreservedBlockers(plan schedPlan, c *capacitySnapshot) map[string]bool {
	cf := counterfactual{c: c, plan: plan, holds: e.unreservedHolds(), usage: e.fairness().usage}
	candidates := map[string]bool{}
	for t := range cf.holds {
		candidates[t] = true
	}
	for _, o := range e.objs {
		for _, p := range o.pods {
			if p.state != laboratoryv1alpha1.PodStarting || e.nodeOf(p) != "" {
				continue
			}
			u := unboundPod{need: podNeed{Key: p.key, CPU: p.need.cpu, Memory: p.need.mem}}
			if objEvictable(o) {
				u.tenant = p.tenant
				candidates[p.tenant] = true
			}
			cf.unbound = append(cf.unbound, u)
		}
	}
	for _, r := range plan.remainders {
		if gangEvictable(r.g) {
			candidates[r.g.tenant] = true
		}
	}
	base := cf.shortfallWithout("")
	out := map[string]bool{}
	for t := range candidates {
		if s := cf.shortfallWithout(t); s.cpu < base.cpu || s.mem < base.mem {
			out[t] = true
		}
	}
	return out
}

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
		if sf.tenants[n] {
			c := sf.condition()
			c.ObservedGeneration, c.LastTransitionTime = t.Generation, metav1.NewTime(now)
			changed = meta.SetStatusCondition(&next.Status.Conditions, c)
		} else {
			changed = meta.RemoveStatusCondition(&next.Status.Conditions, laboratoryv1alpha1.TenantConditionCapacityShortfall)
		}
		if !changed {
			continue
		}
		if at, ok := s.lastWrite[t.UID]; ok && now.Sub(at) < interval {
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
	for _, g := range snap.groups {
		t := names.TenantOf(g.Labels)
		if !evictable(g) || !blockers[t] {
			continue
		}
		foreign := false
		for _, l := range snap.labs {
			if l.Namespace == laboratoryv1alpha1.LabGroupNamespaceOf(g) && (!evictable(l) || names.TenantOf(l.Labels) != t) {
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

// evictUnreserved marks the targets with the evict-at annotation (at most the status budget of new marks per tick), removes
// the mark from every other object that has it, and deletes the targets whose time passed, in one pass after checking past
// the cache.
func (s *Scheduler) evictUnreserved(ctx context.Context, snap *clusterView, blockers map[string]bool, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	targets := evictionTargets(snap, blockers)
	budget := s.Config.StatusBudget
	if budget <= 0 {
		budget = defaultStatusBudget
	}
	objs := make([]client.Object, 0, len(snap.labs)+len(snap.groups))
	for _, l := range snap.labs {
		objs = append(objs, l)
	}
	for _, g := range snap.groups {
		objs = append(objs, g)
	}
	var due []client.Object
	marked := 0
	for _, o := range objs {
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		want := targets[objectKey(o)]
		at, has, ok := evictAt(o)
		switch {
		case want && ok:
			if !now.Before(at) {
				due = append(due, o)
			}
		case want:
			if budget <= 0 {
				continue
			}
			budget--
			when := now.Add(s.unreservedNotice()).UTC().Format(time.RFC3339)
			if err := s.annotate(ctx, o, when); err != nil {
				logger.Error(err, "mark unreserved work for deletion", "object", objectKey(o))
				continue
			}
			marked++
			if s.Recorder != nil {
				s.Recorder.Eventf(o, corev1.EventTypeWarning, "UnreservedEviction",
					"active reservations need the room the unreserved work of tenant %s holds: this object is deleted at %s unless the shortfall clears", names.TenantOf(o.GetLabels()), when)
			}
		case has:
			if err := s.annotate(ctx, o, ""); err != nil {
				logger.Error(err, "remove the eviction mark", "object", objectKey(o))
			}
		}
	}
	if marked > 0 {
		logger.Info("unreserved work holds the room of active reservations: marked for deletion", "objects", marked, "notice", s.unreservedNotice())
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
// object is the one marked (UID), still unreserved, still a target and still due. Each goes with its UID as precondition.
func (s *Scheduler) deleteEvicted(ctx context.Context, due []client.Object, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
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
		uid := cur.GetUID()
		if err := s.Delete(ctx, cur, client.Preconditions{UID: &uid}); err != nil {
			if errors.IsNotFound(err) || errors.IsConflict(err) {
				continue // gone, or another object has the name now
			}
			logger.Error(err, "delete unreserved work", "object", key)
			continue
		}
		logger.Info("deleted unreserved work: active reservations need its room", "object", key, "tenant", names.TenantOf(cur.GetLabels()))
	}
}
