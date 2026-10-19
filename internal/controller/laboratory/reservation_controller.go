package laboratory

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=reservations,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=reservations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs;labgroups,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=tenants,verbs=get;list;watch

const (
	// DefaultReservationNotice is the ending notice of the chart (reservations.endingNoticeSeconds).
	DefaultReservationNotice = 300 * time.Second
	// reservationRetention is how long an ended Reservation stays before it is deleted.
	reservationRetention = 24 * time.Hour
	// reservationRecheck bounds the wait between two looks at a reservation that has not ended (its tenant may go; only
	// Reservations are watched) and at an ended one whose deletion is not complete (a group still holds labs of others).
	reservationRecheck = time.Minute
)

// ReservationReconciler drives the timeline of a Reservation: status.phase (Pending before spec.from, Active, Ending from
// spec.to minus the notice, Ended at spec.to), status.noticeAt and the Ending and Invalid conditions. At the end it deletes
// every Lab and LabGroup of the reservation's tenant that carries its label, and 24 h later the Reservation itself. A
// reservation whose tenant is gone ends one notice after that was seen. status.used and status.groups belong to the
// Scheduler: every write here is a merge patch of the fields above only, with the resourceVersion as precondition.
type ReservationReconciler struct {
	client.Client
	// Notice is how long before the end the Ending condition is set.
	Notice time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (r *ReservationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ReservationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// The scheduler writes status.used on every change of usage; only spec changes (and the milestones) matter here.
		For(&laboratoryv1alpha1.Reservation{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("reservation").
		Complete(reconcileutil.Quiet(r))
}

// reservationTimeline is where a reservation stands on its clock.
type reservationTimeline struct {
	phase    laboratoryv1alpha1.ReservationPhase
	noticeAt *metav1.Time
	// reason is the reason of the Ending condition (WindowClosing or TenantGone while ending, Ended or TenantGone after).
	reason string
	// end is when its groups are deleted.
	end time.Time
	// next is the next milestone.
	next time.Time
}

// timeline computes the phase from the clock, the tenant and what the status says was given already. The ending notice
// keeps the time it was first given. A reservation that ended because its tenant was gone stays ended.
func (r *ReservationReconciler) timeline(res *laboratoryv1alpha1.Reservation, tenantGone bool, now time.Time) reservationTimeline {
	from, to, n := res.Spec.From.Time, res.Spec.To.Time, r.Notice
	st := res.Status
	ending := meta.FindStatusCondition(st.Conditions, laboratoryv1alpha1.ReservationConditionEnding)
	given := func() *metav1.Time {
		if st.NoticeAt != nil && st.Phase == laboratoryv1alpha1.ReservationPhaseEnding {
			return st.NoticeAt
		}
		t := metav1.NewTime(now)
		return &t
	}
	if st.Phase == laboratoryv1alpha1.ReservationPhaseEnded && ending != nil && ending.Reason == laboratoryv1alpha1.ReservationReasonTenantGone {
		end := to
		if st.NoticeAt != nil && st.NoticeAt.Add(n).Before(end) {
			end = st.NoticeAt.Add(n)
		}
		return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhaseEnded, noticeAt: st.NoticeAt, reason: laboratoryv1alpha1.ReservationReasonTenantGone, end: end, next: end.Add(reservationRetention)}
	}
	if tenantGone {
		notice := given()
		end := to
		if notice.Add(n).Before(end) {
			end = notice.Add(n)
		}
		if now.Before(end) {
			return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhaseEnding, noticeAt: notice, reason: laboratoryv1alpha1.ReservationReasonTenantGone, end: end, next: end}
		}
		return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhaseEnded, noticeAt: notice, reason: laboratoryv1alpha1.ReservationReasonTenantGone, end: end, next: end.Add(reservationRetention)}
	}
	switch {
	case !now.Before(to):
		return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhaseEnded, noticeAt: st.NoticeAt, reason: laboratoryv1alpha1.ReservationReasonEnded, end: to, next: to.Add(reservationRetention)}
	case !now.Before(to.Add(-n)):
		return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhaseEnding, noticeAt: given(), reason: laboratoryv1alpha1.ReservationReasonWindowClosing, end: to, next: to}
	case now.Before(from):
		next := from
		if to.Add(-n).Before(next) {
			next = to.Add(-n)
		}
		return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhasePending, end: to, next: next}
	default:
		return reservationTimeline{phase: laboratoryv1alpha1.ReservationPhaseActive, end: to, next: to.Add(-n)}
	}
}

func (r *ReservationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var res laboratoryv1alpha1.Reservation
	if err := r.Get(ctx, req.NamespacedName, &res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if res.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	now := r.now()
	gone, err := r.tenantGone(ctx, res.Spec.Tenant)
	if err != nil {
		return ctrl.Result{}, err
	}
	tl := r.timeline(&res, gone, now)
	incomplete := false
	if tl.phase == laboratoryv1alpha1.ReservationPhaseEnded {
		if incomplete, err = r.deleteGroups(ctx, &res); err != nil {
			return ctrl.Result{}, err
		}
		if !now.Before(tl.end.Add(reservationRetention)) {
			uid := res.UID
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &res, client.Preconditions{UID: &uid}))
		}
	}
	if err := r.writeStatus(ctx, &res, tl, now); err != nil {
		return ctrl.Result{}, err
	}
	next := tl.next
	if (tl.phase != laboratoryv1alpha1.ReservationPhaseEnded && !gone) || incomplete {
		if check := now.Add(reservationRecheck); check.Before(next) {
			next = check
		}
	}
	after := next.Sub(now)
	if after < time.Second {
		after = time.Second
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// tenantGone says the reservation's tenant no longer exists (or is being deleted).
func (r *ReservationReconciler) tenantGone(ctx context.Context, name string) (bool, error) {
	var t laboratoryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return t.DeletionTimestamp != nil, nil
}

// ownedBy says an object runs under the reservation: it carries its label and belongs to its tenant (an object of another
// tenant with the label never ran under it, see reservationLedger, and is not this reservation's to delete).
func ownedBy(res *laboratoryv1alpha1.Reservation, labels map[string]string) bool {
	return labels[names.LabelReservation] == res.Name && names.TenantOf(labels) == res.Spec.Tenant
}

// deleteGroups deletes, in one pass, every LabGroup and Lab of the reservation that is not being deleted yet, each with its
// UID as precondition. A LabGroup whose namespace still holds labs that are not the reservation's is left (deleting it would
// drain them): incomplete is true then, and the pass is repeated until those labs are gone.
func (r *ReservationReconciler) deleteGroups(ctx context.Context, res *laboratoryv1alpha1.Reservation) (incomplete bool, err error) {
	sel := client.MatchingLabels{names.LabelReservation: res.Name}
	var groups laboratoryv1alpha1.LabGroupList
	if err := r.List(ctx, &groups, sel); err != nil {
		return false, err
	}
	for i := range groups.Items {
		g := &groups.Items[i]
		if g.DeletionTimestamp != nil || !ownedBy(res, g.Labels) {
			continue
		}
		foreign, err := r.holdsForeignLabs(ctx, res, g)
		if err != nil {
			return false, err
		}
		if foreign {
			incomplete = true
			continue
		}
		again, err := r.deleteExact(ctx, g)
		if err != nil {
			return false, err
		}
		incomplete = incomplete || again
	}
	var labs laboratoryv1alpha1.LabList
	if err := r.List(ctx, &labs, sel); err != nil {
		return false, err
	}
	for i := range labs.Items {
		l := &labs.Items[i]
		if l.DeletionTimestamp != nil || !ownedBy(res, l.Labels) {
			continue
		}
		again, err := r.deleteExact(ctx, l)
		if err != nil {
			return false, err
		}
		incomplete = incomplete || again
	}
	return incomplete, nil
}

// holdsForeignLabs says the namespace of the group holds a lab, not being deleted, that is not the reservation's.
func (r *ReservationReconciler) holdsForeignLabs(ctx context.Context, res *laboratoryv1alpha1.Reservation, g *laboratoryv1alpha1.LabGroup) (bool, error) {
	var labs laboratoryv1alpha1.LabList
	if err := r.List(ctx, &labs, client.InNamespace(laboratoryv1alpha1.LabGroupNamespaceOf(g))); err != nil {
		return false, err
	}
	for i := range labs.Items {
		if l := &labs.Items[i]; l.DeletionTimestamp == nil && !ownedBy(res, l.Labels) {
			return true, nil
		}
	}
	return false, nil
}

// deleteExact deletes exactly the object that was listed (UID precondition). Gone is done; a conflict (another object now has
// the name) asks for another pass.
func (r *ReservationReconciler) deleteExact(ctx context.Context, o client.Object) (again bool, err error) {
	uid := o.GetUID()
	switch err := r.Delete(ctx, o, client.Preconditions{UID: &uid}); {
	case err == nil, apierrors.IsNotFound(err):
		return false, nil
	case apierrors.IsConflict(err):
		return true, nil
	default:
		return false, err
	}
}

// writeStatus patches phase, noticeAt and the Ending and Invalid conditions when they changed. The merge patch carries only
// these fields and the resourceVersion, so the scheduler's status.used and status.groups are never overwritten and a write in
// between makes this one conflict and run again.
func (r *ReservationReconciler) writeStatus(ctx context.Context, res *laboratoryv1alpha1.Reservation, tl reservationTimeline, now time.Time) error {
	orig := res.DeepCopy()
	st := &res.Status
	changed := st.Phase != tl.phase || !noticeEqual(st.NoticeAt, tl.noticeAt)
	st.Phase, st.NoticeAt = tl.phase, tl.noticeAt
	set := func(c metav1.Condition) {
		c.ObservedGeneration, c.LastTransitionTime = res.Generation, metav1.NewTime(now)
		if meta.SetStatusCondition(&st.Conditions, c) {
			changed = true
		}
	}
	end := tl.end.UTC().Format(time.RFC3339)
	switch tl.phase {
	case laboratoryv1alpha1.ReservationPhaseEnding:
		msg := fmt.Sprintf("the reservation ends at %s; its groups are deleted then", end)
		if tl.reason == laboratoryv1alpha1.ReservationReasonTenantGone {
			msg = fmt.Sprintf("the tenant %s is gone: the reservation ends at %s and its groups are deleted then", res.Spec.Tenant, end)
		}
		set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionEnding, Status: metav1.ConditionTrue, Reason: tl.reason, Message: msg})
	case laboratoryv1alpha1.ReservationPhaseEnded:
		msg := fmt.Sprintf("the reservation ended at %s; its groups are deleted", end)
		if tl.reason == laboratoryv1alpha1.ReservationReasonTenantGone {
			msg = fmt.Sprintf("the tenant %s is gone: the reservation ended at %s; its groups are deleted", res.Spec.Tenant, end)
		}
		set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionEnding, Status: metav1.ConditionFalse, Reason: tl.reason, Message: msg})
	default:
		if meta.FindStatusCondition(st.Conditions, laboratoryv1alpha1.ReservationConditionEnding) != nil {
			set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionEnding, Status: metav1.ConditionFalse, Reason: laboratoryv1alpha1.ReservationReasonExtended,
				Message: fmt.Sprintf("the end was called off: the reservation ends at %s", end)})
		}
	}
	if problem := reservationProblem(res); problem != "" {
		set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionInvalid, Status: metav1.ConditionTrue, Reason: "InvalidResources", Message: problem + "; the reservation reserves nothing"})
	} else if meta.RemoveStatusCondition(&st.Conditions, laboratoryv1alpha1.ReservationConditionInvalid) {
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, res, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

func noticeEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}
