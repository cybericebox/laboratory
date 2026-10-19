package grpc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/client"
)

// The agent's side of decision 17: every LabGroup and Lab runs under a reservation of its tenant, and one that does not fit
// the rest of its reservation is refused early ("not enough room in this reservation"). The operator stays the guarantee:
// calls of other agent replicas may pass the check together, and the operator then holds the extra item (ReservationFull).

// fitItem is one object a create call adds under a reservation, in request order.
type fitItem struct {
	// reservation is the id as given; "" = none.
	reservation string
	need        nodecap.Amount
	// deployGroup is the deploy group label (names.DeployKey); "" for an object of its own.
	deployGroup string
	// exists: an object with this name exists, so the create is idempotent and adds nothing (no fit check).
	exists bool
	// skip: the item is refused for another reason already and takes no room.
	skip bool
}

// reservationFit is a reservation as the fit check sees it.
type reservationFit struct {
	name              string
	res               *laboratoryv1alpha1.Reservation // nil: there is none of the tenant
	amount, committed nodecap.Amount
	ended             bool
}

// labNeed is what a lab's pods request, as the scheduler counts them.
func labNeed(spec *laboratoryv1alpha1.LabSpec, defaults limits.DeviceDefaults) nodecap.Amount {
	cpu, mem := limits.LabNeed(spec, defaults)
	return nodecap.Amount{CPU: cpu, Memory: mem}
}

// reservationEnded says the reservation takes no more work: it is being deleted, ended, past its end, or its tenant is gone.
// A Pending one takes work (the operator holds it until the window opens).
func reservationEnded(r *laboratoryv1alpha1.Reservation, now time.Time) bool {
	if r.DeletionTimestamp != nil || r.Status.Phase == laboratoryv1alpha1.ReservationPhaseEnded || !now.Before(r.Spec.To.Time) {
		return true
	}
	c := meta.FindStatusCondition(r.Status.Conditions, laboratoryv1alpha1.ReservationConditionEnding)
	return c != nil && c.Reason == laboratoryv1alpha1.ReservationReasonTenantGone
}

// lockReservations takes the fit locks of the reservations (in name order, so two calls never wait on each other crosswise)
// and returns the unlock.
func (h *Handler) lockReservations(ids []string) func() {
	keys := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if k := "reservation/" + crName(id); id != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	unlocks := make([]func(), 0, len(keys))
	for _, k := range keys {
		unlocks = append(unlocks, h.keyLock(k))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

// loadReservationFits reads the reservations by id and what their objects already take: the requests of every LabGroup and
// Lab of the tenant that carries the reservation's label and is neither being deleted nor stopped (counted on the definition,
// so queued objects count too).
func (h *Handler) loadReservationFits(ctx context.Context, tenantName string, ids []string) (map[string]*reservationFit, error) {
	out := map[string]*reservationFit{}
	now := h.now()
	for _, id := range ids {
		if id == "" || out[id] != nil {
			continue
		}
		f := &reservationFit{name: crName(id)}
		out[id] = f
		r, err := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, f.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !ownsReservation(tenantName, r) {
			continue
		}
		f.res, f.ended = r, reservationEnded(r, now)
		if reservationProblem(r) == "" {
			w := windowOf(r)
			f.amount = nodecap.Amount{CPU: w.cpu, Memory: w.mem}
		}
		sel := metav1.ListOptions{LabelSelector: names.LabelReservation + "=" + f.name}
		groups, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, sel)
		if err != nil {
			return nil, err
		}
		for i := range groups.Items {
			g := &groups.Items[i]
			if names.TenantOf(g.Labels) == tenantName && g.DeletionTimestamp == nil && !g.Spec.Lifecycle.IsStopped() {
				f.committed = addAmount(f.committed, h.groupNeedOf(g))
			}
		}
		labs, err := h.cs.LaboratoryV1alpha1().Labs(metav1.NamespaceAll).List(ctx, sel)
		if err != nil {
			return nil, err
		}
		for i := range labs.Items {
			l := &labs.Items[i]
			if names.TenantOf(l.Labels) == tenantName && l.DeletionTimestamp == nil && !l.Spec.Lifecycle.IsStopped() {
				f.committed = addAmount(f.committed, labNeed(&l.Spec, h.features.DeviceDefaults))
			}
		}
	}
	return out, nil
}

// reservationProblem says why a reservation's amount reserves nothing ("" when it is fine), as the operator judges it: cpu
// and memory must both be there and positive.
func reservationProblem(r *laboratoryv1alpha1.Reservation) string {
	w := windowOf(r)
	if w.cpu <= 0 || w.mem <= 0 || len(r.Spec.Resources) != 2 {
		return "the reservation reserves nothing"
	}
	return ""
}

// groupNeedOf is what a group's own pods (VPN, gateway) request: their sizes, else the chart's defaults.
func (h *Handler) groupNeedOf(lg *laboratoryv1alpha1.LabGroup) nodecap.Amount {
	o := h.groupPods.Need(lg)
	return nodecap.Amount{CPU: o.CPU, Memory: o.Memory}
}

func addAmount(a, b nodecap.Amount) nodecap.Amount {
	return nodecap.Amount{CPU: a.CPU + b.CPU, Memory: a.Memory + b.Memory}
}

// deployGroupReservations are the reservation labels ("" for none) of the tenant's live LabGroups and Labs in each of the
// deploy groups.
func (h *Handler) deployGroupReservations(ctx context.Context, tenantName string, keys []string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	sel := metav1.ListOptions{LabelSelector: names.LabelDeployGroup + " in (" + strings.Join(keys, ",") + ")"}
	add := func(o metav1.Object) {
		if names.TenantOf(o.GetLabels()) != tenantName || o.GetDeletionTimestamp() != nil {
			return
		}
		k := o.GetLabels()[names.LabelDeployGroup]
		if out[k] == nil {
			out[k] = map[string]bool{}
		}
		out[k][o.GetLabels()[names.LabelReservation]] = true
	}
	groups, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, sel)
	if err != nil {
		return nil, err
	}
	for i := range groups.Items {
		add(&groups.Items[i])
	}
	labs, err := h.cs.LaboratoryV1alpha1().Labs(metav1.NamespaceAll).List(ctx, sel)
	if err != nil {
		return nil, err
	}
	for i := range labs.Items {
		add(&labs.Items[i])
	}
	return out, nil
}

// reservationRefusals applies the reservation rules to the items, in request order, and returns the refusal of each (nil:
// it may go on). The room an item takes counts for the items after it.
//  1. no reservation_id: RESERVATION_REQUIRED;
//  2. the reservation is not the tenant's: RESERVATION_NOT_FOUND;
//  3. it takes no more work (reservationEnded): RESERVATION_ENDED;
//  4. its deploy group already runs, or is created in this call, under another reservation: RESERVATION_MISMATCH (the
//     operator would refuse the whole gang as mixed);
//  5. it does not fit what is left of the reservation: RESERVATION_FULL.
//
// An item whose object exists is idempotent and skips the fit.
func (h *Handler) reservationRefusals(ctx context.Context, tenantName string, items []fitItem) ([]error, error) {
	out := make([]error, len(items))
	ids := make([]string, 0, len(items))
	var keys []string
	seenKey := map[string]bool{}
	for _, it := range items {
		ids = append(ids, it.reservation)
		if it.deployGroup != "" && !seenKey[it.deployGroup] {
			seenKey[it.deployGroup] = true
			keys = append(keys, it.deployGroup)
		}
	}
	fits, err := h.loadReservationFits(ctx, tenantName, ids)
	if err != nil {
		return nil, err
	}
	existing, err := h.deployGroupReservations(ctx, tenantName, keys)
	if err != nil {
		return nil, err
	}
	inCall := map[string]string{} // deploy group -> reservation label of the first item that passed
	for i, it := range items {
		if it.skip {
			continue
		}
		if it.reservation == "" {
			if !h.reservationsOptional {
				out[i] = refusal(client.ReasonReservationRequired, "a reservation id is required")
			}
			continue
		}
		f := fits[it.reservation]
		switch {
		case f.res == nil:
			out[i] = refusal(client.ReasonReservationNotFound, "reservation %q not found", it.reservation)
			continue
		case f.ended:
			out[i] = refusal(client.ReasonReservationEnded, "reservation %q takes no more work: it has ended or is ending for good", it.reservation)
			continue
		}
		if it.deployGroup != "" {
			mismatch := false
			for label := range existing[it.deployGroup] {
				mismatch = mismatch || label != f.name
			}
			if prev, ok := inCall[it.deployGroup]; ok && prev != f.name {
				mismatch = true
			}
			if mismatch {
				out[i] = refusal(client.ReasonReservationMismatch, "the deploy group runs under another reservation: a deploy group runs under one reservation")
				continue
			}
			inCall[it.deployGroup] = f.name
		}
		if it.exists {
			continue
		}
		next := addAmount(f.committed, it.need)
		if next.CPU > f.amount.CPU || next.Memory > f.amount.Memory {
			left := nodecap.Amount{CPU: max(f.amount.CPU-f.committed.CPU, 0), Memory: max(f.amount.Memory-f.committed.Memory, 0)}
			out[i] = refusal(client.ReasonReservationFull, "not enough room in this reservation (needs cpu %dm memory %d, left cpu %dm memory %d)",
				it.need.CPU, it.need.Memory, left.CPU, left.Memory)
			continue
		}
		f.committed = next
	}
	return out, nil
}

// attachReservation checks an attach (Update with reservation_id) of an object with these labels and returns the label value
// to set; "" when it already runs under that reservation. Only an object without a reservation may be attached, to a
// reservation of its tenant that takes work; there is no fit check (running work is never refused).
func (h *Handler) attachReservation(ctx context.Context, kind, name string, labels map[string]string, id string) (string, error) {
	want := crName(id)
	if cur, has := names.ReservationOf(labels); has {
		if cur == want {
			return "", nil
		}
		return "", refusal(client.ReasonReservationMismatch, "%s %s already runs under another reservation", kind, name)
	}
	r, err := h.getReservation(ctx, id)
	if apierrors.IsNotFound(err) {
		return "", refusal(client.ReasonReservationNotFound, "reservation %q not found", id)
	}
	if err != nil {
		return "", err
	}
	if reservationEnded(r, h.now()) {
		return "", refusal(client.ReasonReservationEnded, "reservation %q takes no more work: it has ended or is ending for good", id)
	}
	return want, nil
}

// existingReservationLabel refuses a create that would put an existing object under another reservation than the one it
// runs under.
func existingReservationLabel(kind, name string, cur map[string]string, want string) error {
	if have, ok := names.ReservationOf(cur); ok && want != "" && have != want {
		return refusal(client.ReasonReservationMismatch, "%s %s already runs under another reservation", kind, name)
	}
	return nil
}

func reservationIDError(id string) error {
	if id == "" {
		return nil
	}
	if err := names.ValidateID(id); err != nil {
		return fmt.Errorf("reservation_id: %w", err)
	}
	return nil
}
