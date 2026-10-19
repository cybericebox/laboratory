package laboratory

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// This file is the usage side of the reservation timeline (spec item 14, "a reservation is exceeded"): the groups of a
// reservation are deleted only when the backend made it smaller than what runs under it. Usage over the amount for any other
// reason (device defaults changed, a Lab edited, a label moved, all recounted by the scheduler) is shown, never acted on.

// fitPrefix starts the record of the last fit in the message of the WithinAmount condition: the generation of the reservation
// and its amount when status.used was last seen within it, or when a shrink was served (its groups deleted: that amount is the
// new baseline). The status has no other field to carry it.
const fitPrefix = "last fit: "

// fitRecord is the generation and amount at which the usage was last within the amount.
type fitRecord struct {
	gen int64
	amt amount
}

func (f fitRecord) String() string {
	return fmt.Sprintf("%sgeneration=%d cpu=%dm memory=%d", fitPrefix, f.gen, f.amt.cpu, f.amt.mem)
}

// lastFit reads the record of the last fit from the WithinAmount condition; false when there is none (or it cannot be read,
// which then never arms a deletion).
func lastFit(res *laboratoryv1alpha1.Reservation) (fitRecord, bool) {
	c := meta.FindStatusCondition(res.Status.Conditions, laboratoryv1alpha1.ReservationConditionWithinAmount)
	if c == nil {
		return fitRecord{}, false
	}
	i := strings.Index(c.Message, fitPrefix)
	if i < 0 {
		return fitRecord{}, false
	}
	var f fitRecord
	if _, err := fmt.Sscanf(c.Message[i:], fitPrefix+"generation=%d cpu=%dm memory=%d", &f.gen, &f.amt.cpu, &f.amt.mem); err != nil {
		return fitRecord{}, false
	}
	return f, true
}

// usedOf is status.used as the scheduler wrote it (nothing written: nothing used).
func usedOf(res *laboratoryv1alpha1.Reservation) amount {
	u := res.Status.Used
	if u == nil {
		return amount{}
	}
	return amount{cpu: u.Cpu().MilliValue(), mem: u.Memory().Value()}
}

// usageView is how status.used stands against the amount.
type usageView struct {
	// over says what is over the amount; "" when the usage fits.
	over string
	// shrunk: over because the backend made the amount smaller since the last fit (a newer generation, and in a resource that
	// is over the amount went down while the usage still fits the recorded amount: usage over the recorded amount was drift
	// before the shrink and is never its doing). Never for an invalid reservation (its spec is wrong, it was not made smaller).
	shrunk bool
	last   fitRecord
	known  bool
}

func usageOf(res *laboratoryv1alpha1.Reservation) usageView {
	a, u := reservationAmount(res), usedOf(res)
	v := usageView{}
	v.last, v.known = lastFit(res)
	if u.cpu <= a.cpu && u.mem <= a.mem {
		return v
	}
	v.over = fmt.Sprintf("the groups use cpu %dm and memory %d bytes, more than the reserved cpu %dm and memory %d bytes", u.cpu, u.mem, a.cpu, a.mem)
	v.shrunk = reservationProblem(res) == "" && v.known && res.Generation > v.last.gen &&
		((u.cpu > a.cpu && a.cpu < v.last.amt.cpu && u.cpu <= v.last.amt.cpu) || (u.mem > a.mem && a.mem < v.last.amt.mem && u.mem <= v.last.amt.mem))
	return v
}

// groupsDeleted says the groups of the reservation were already deleted for an excess at its current generation: the same
// shrink never deletes twice (status.used may stay stale, e.g. without a running scheduler).
func groupsDeleted(res *laboratoryv1alpha1.Reservation) bool {
	c := meta.FindStatusCondition(res.Status.Conditions, laboratoryv1alpha1.ReservationConditionExceeded)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == laboratoryv1alpha1.ReservationReasonGroupsDeleted && c.ObservedGeneration == res.Generation
}

// armedExceeded is the Exceeded condition when it is armed by this rule (True, AmountReduced); an older True one (another
// reason) is not: its time is not a notice of this rule.
func armedExceeded(res *laboratoryv1alpha1.Reservation) *metav1.Condition {
	c := meta.FindStatusCondition(res.Status.Conditions, laboratoryv1alpha1.ReservationConditionExceeded)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != laboratoryv1alpha1.ReservationReasonAmountReduced {
		return nil
	}
	return c
}

// servedShrink marks the shrink of the reservation served after its groups were deleted: Exceeded False (GroupsDeleted) and
// the WithinAmount record moved to the current generation and amount, so the same excess (stale usage, a later unrelated
// spec change) never arms again; only a further shrink does.
func servedShrink(res *laboratoryv1alpha1.Reservation, notice metav1.Time, now time.Time) {
	t := metav1.NewTime(now)
	meta.SetStatusCondition(&res.Status.Conditions, metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionExceeded, Status: metav1.ConditionFalse,
		Reason: laboratoryv1alpha1.ReservationReasonGroupsDeleted, ObservedGeneration: res.Generation, LastTransitionTime: t,
		Message: fmt.Sprintf("the groups created before %s were deleted: the reservation was made smaller than what ran under it", notice.UTC().Format(time.RFC3339))})
	meta.SetStatusCondition(&res.Status.Conditions, metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionWithinAmount, Status: metav1.ConditionFalse,
		Reason: "OverAmount", ObservedGeneration: res.Generation, LastTransitionTime: t,
		Message: "the usage is over the amount; " + fitRecord{gen: res.Generation, amt: reservationAmount(res)}.String()})
}

// usagePlan is what the usage conditions say in this pass.
type usagePlan struct {
	// on: the usage is looked at (the scheduler runs and the reservation has not ended).
	on bool
	v  usageView
	// armed: Exceeded is True; its groups created before the notice go at due.
	armed bool
	due   time.Time
	// stale: due, but status.usedAt is not fresh (the scheduler has not counted the usage lately): the deletion waits.
	stale bool
}

// deleteNow says the Exceeded deletion is due and the usage it rests on is fresh.
func (p usagePlan) deleteNow(now time.Time) bool { return p.armed && !now.Before(p.due) && !p.stale }

// usageFresh says the scheduler counted status.used less than maxAge ago (status.usedAt).
func usageFresh(res *laboratoryv1alpha1.Reservation, now time.Time, maxAge time.Duration) bool {
	at := res.Status.UsedAt
	return at != nil && now.Sub(at.Time) < maxAge
}

func (r *ReservationReconciler) usagePlan(res *laboratoryv1alpha1.Reservation, tl reservationTimeline, now time.Time) usagePlan {
	if r.NoScheduler || tl.phase == laboratoryv1alpha1.ReservationPhaseEnded {
		return usagePlan{}
	}
	p := usagePlan{on: true, v: usageOf(res)}
	open := tl.phase == laboratoryv1alpha1.ReservationPhaseActive || tl.phase == laboratoryv1alpha1.ReservationPhaseEnding
	if open && p.v.shrunk && !groupsDeleted(res) {
		p.armed = true
		p.due = now.Add(r.lossNotice())
		if c := armedExceeded(res); c != nil {
			p.due = c.LastTransitionTime.Add(r.lossNotice())
		}
		p.stale = !now.Before(p.due) && !usageFresh(res, now, r.usageMaxAge())
	}
	return p
}

// setUsageConditions writes WithinAmount, Exceeded and OverAmount for the plan; set and drop change the conditions and say
// whether they changed.
func setUsageConditions(res *laboratoryv1alpha1.Reservation, p usagePlan, set func(metav1.Condition), drop func(string)) {
	if !p.on {
		drop(laboratoryv1alpha1.ReservationConditionExceeded)
		drop(laboratoryv1alpha1.ReservationConditionOverAmount)
		return
	}
	if p.v.over == "" {
		set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionWithinAmount, Status: metav1.ConditionTrue, Reason: "Fits",
			Message: "the usage is within the amount; " + fitRecord{gen: res.Generation, amt: reservationAmount(res)}.String()})
		drop(laboratoryv1alpha1.ReservationConditionExceeded)
		drop(laboratoryv1alpha1.ReservationConditionOverAmount)
		return
	}
	last := "it never fit"
	if p.v.known {
		last = p.v.last.String()
	}
	set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionWithinAmount, Status: metav1.ConditionFalse, Reason: "OverAmount",
		Message: "the usage is over the amount; " + last})
	if p.armed {
		if armedExceeded(res) == nil {
			drop(laboratoryv1alpha1.ReservationConditionExceeded) // a True one of another reason: the notice starts now
		}
		msg := fmt.Sprintf("the reservation was made smaller than what runs under it: %s; its groups created before this notice are deleted at %s unless the usage or the reservation changes",
			p.v.over, p.due.UTC().Format(time.RFC3339))
		switch {
		case p.stale && res.Status.Used != nil && res.Status.UsedAt == nil:
			// the scheduler always writes both: the server drops usedAt
			msg += "; the deletion is held: usage not fresh (status.usedAt is missing: the Reservation CRD is outdated, apply the chart CRDs)"
		case p.stale:
			msg += "; the deletion is held: usage not fresh (the scheduler has not counted it lately)"
		}
		set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionExceeded, Status: metav1.ConditionTrue, Reason: laboratoryv1alpha1.ReservationReasonAmountReduced, Message: msg})
		drop(laboratoryv1alpha1.ReservationConditionOverAmount)
		return
	}
	if !groupsDeleted(res) {
		drop(laboratoryv1alpha1.ReservationConditionExceeded)
	}
	set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionOverAmount, Status: metav1.ConditionTrue, Reason: "UsageOverAmount",
		Message: p.v.over + "; the reservation was not made smaller, so nothing is deleted for it"})
}
