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
// and its amount when status.used was last seen within it. The status has no other field to carry it.
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
	// shrunk: over because the backend made the amount smaller since the last fit (a newer generation, and the amount went
	// down in a resource that is over). Never for an invalid reservation (its spec is wrong, it was not made smaller).
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
		((u.cpu > a.cpu && a.cpu < v.last.amt.cpu) || (u.mem > a.mem && a.mem < v.last.amt.mem))
	return v
}

// groupsDeleted says the groups of the reservation were already deleted for an excess at its current generation: the same
// shrink never deletes twice (status.used may stay stale, e.g. without a running scheduler).
func groupsDeleted(res *laboratoryv1alpha1.Reservation) bool {
	c := meta.FindStatusCondition(res.Status.Conditions, laboratoryv1alpha1.ReservationConditionExceeded)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == laboratoryv1alpha1.ReservationReasonGroupsDeleted && c.ObservedGeneration == res.Generation
}

// usagePlan is what the usage conditions say in this pass.
type usagePlan struct {
	// on: the usage is looked at (the scheduler runs and the reservation has not ended).
	on bool
	v  usageView
	// armed: Exceeded is True; its groups created before the notice go at due.
	armed bool
	due   time.Time
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
		if c := meta.FindStatusCondition(res.Status.Conditions, laboratoryv1alpha1.ReservationConditionExceeded); c != nil && c.Status == metav1.ConditionTrue {
			p.due = c.LastTransitionTime.Add(r.lossNotice())
		}
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
		set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionExceeded, Status: metav1.ConditionTrue, Reason: laboratoryv1alpha1.ReservationReasonAmountReduced,
			Message: fmt.Sprintf("the reservation was made smaller than what runs under it: %s; its groups created before this notice are deleted at %s unless the usage or the reservation changes",
				p.v.over, p.due.UTC().Format(time.RFC3339))})
		drop(laboratoryv1alpha1.ReservationConditionOverAmount)
		return
	}
	if !groupsDeleted(res) {
		drop(laboratoryv1alpha1.ReservationConditionExceeded)
	}
	set(metav1.Condition{Type: laboratoryv1alpha1.ReservationConditionOverAmount, Status: metav1.ConditionTrue, Reason: "UsageOverAmount",
		Message: p.v.over + "; the reservation was not made smaller, so nothing is deleted for it"})
}
