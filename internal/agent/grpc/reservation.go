package grpc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/tenant"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	kindReservation = "Reservation"
	// maxReservationWindow is the longest window of one reservation.
	maxReservationWindow = 31 * 24 * time.Hour
	// defaultReservationNotice is the chart's reservations.endingNoticeSeconds, for an agent not told otherwise.
	defaultReservationNotice = 300 * time.Second
	// reservationLossFloor is the operator's floor of the notice of an end that one observation starts (the tenant gone, a
	// shrink): reservationRecheck of the ReservationReconciler.
	reservationLossFloor = time.Minute
)

// SetReservationNotice sets the ending notice of the reservations (the chart's reservations.endingNoticeSeconds), which the
// operator uses too: the agent reports it and computes the end of a reservation's groups from it.
func (h *Handler) SetReservationNotice(d time.Duration) { h.resNotice, h.resNoticeSet = d, true }

func (h *Handler) reservationNotice() time.Duration {
	if h.resNoticeSet {
		return h.resNotice
	}
	return defaultReservationNotice
}

// keyLock serializes the work on one key (a tenant's reservations, one reservation's fit) inside this agent process.
func (h *Handler) keyLock(key string) (unlock func()) {
	v, _ := h.resLocks.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// reasonError is a refusal with a machine code: ItemResult.reason.
type reasonError struct {
	reason    string
	text      string
	retryable bool
}

func (e reasonError) Error() string { return e.text }

func refusal(reason, format string, a ...any) error {
	return reasonError{reason: reason, text: fmt.Sprintf(format, a...), retryable: reason == client.ReasonReservationFull}
}

// reasonOf is the machine code of a failure, "" for one without.
func reasonOf(err error) string {
	var re reasonError
	if errors.As(err, &re) {
		return re.reason
	}
	return ""
}

func reservationRef(id string) *protobuf.ItemRef { return &protobuf.ItemRef{Name: id} }

// ownsReservation says the reservation belongs to the tenant: spec.tenant is what the operator goes by.
func ownsReservation(tenantName string, r *laboratoryv1alpha1.Reservation) bool {
	return r.Spec.Tenant == tenantName
}

// listReservations lists the reservations of the caller's tenant that match a (user) selector.
func (h *Handler) listReservations(ctx context.Context, selector string) ([]laboratoryv1alpha1.Reservation, error) {
	t := tenantOf(ctx)
	list, err := h.cs.LaboratoryV1alpha1().Reservations().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for i := range list.Items {
		if ownsReservation(t, &list.Items[i]) {
			out = append(out, list.Items[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// getReservation reads a reservation by id; another tenant's is NotFound.
func (h *Handler) getReservation(ctx context.Context, id string) (*laboratoryv1alpha1.Reservation, error) {
	r, err := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, crName(id), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !ownsReservation(tenantOf(ctx), r) {
		return nil, notFoundForeign(kindReservation, id)
	}
	return r, nil
}

// reservationWindow is a reservation's window and amount as the agent keeps them: times to the second (what the API server
// stores), cpu in millicores, memory in bytes.
type reservationWindow struct {
	from, to time.Time
	cpu, mem int64
}

func unixMs(ms int64) time.Time { return time.UnixMilli(ms).UTC().Truncate(time.Second) }

func windowOf(r *laboratoryv1alpha1.Reservation) reservationWindow {
	return reservationWindow{from: r.Spec.From.UTC(), to: r.Spec.To.UTC(), cpu: r.Spec.Resources.Cpu().MilliValue(), mem: r.Spec.Resources.Memory().Value()}
}

func (w reservationWindow) apply(spec *laboratoryv1alpha1.ReservationSpec) {
	spec.From, spec.To = metav1.NewTime(w.from), metav1.NewTime(w.to)
	spec.Resources = corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(w.cpu, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(w.mem, resource.BinarySI),
	}
}

// validateReservationWindow checks a window and amount: to after from and in the future, at most maxReservationWindow long,
// cpu and memory positive.
func validateReservationWindow(w reservationWindow, now time.Time) error {
	switch {
	case !w.to.After(w.from):
		return fmt.Errorf("to must be after from")
	case !w.to.After(now):
		return fmt.Errorf("to must be in the future")
	case w.to.Sub(w.from) > maxReservationWindow:
		return fmt.Errorf("the window is longer than %s", maxReservationWindow)
	case w.cpu <= 0 || w.mem <= 0:
		return fmt.Errorf("cpu_millicores and memory_bytes must be positive")
	}
	return nil
}

func itemWindow(it *protobuf.ReservationItem) reservationWindow {
	return reservationWindow{from: unixMs(it.GetFromUnixMs()), to: unixMs(it.GetToUnixMs()), cpu: it.GetCpuMillicores(), mem: it.GetMemoryBytes()}
}

// reservationQuota is the caller's tenant quota as the reservations are checked against it: resolved against the maximum (a
// percentage of the lab nodes and the budget), with no limit for a tenant without a quota.
func (h *Handler) reservationQuota(ctx context.Context, name string) (tenant.Limits, error) {
	ten, err := h.tenantObject(ctx, name)
	if err != nil {
		return tenant.Limits{}, err
	}
	return h.resolvedQuota(ctx, ten)
}

// tenantPeakFits refuses (OVER_QUOTA) a candidate window when, together with the tenant's other reservations that overlap
// it, the most reserved at the same time from now on would exceed the quota. Ended reservations, ones being deleted and the
// candidate's own old version (same name) do not count; a tenant without a quota is not checked.
func tenantPeakFits(quota tenant.Limits, have []laboratoryv1alpha1.Reservation, name string, cand reservationWindow, now time.Time) error {
	if !quota.HasCPU && !quota.HasMemory {
		return nil
	}
	from := cand.from
	if from.Before(now) {
		from = now
	}
	spans := []reservationWindow{cand}
	for i := range have {
		r := &have[i]
		if r.Name == name || r.DeletionTimestamp != nil || r.Status.Phase == laboratoryv1alpha1.ReservationPhaseEnded {
			continue
		}
		spans = append(spans, windowOf(r))
	}
	peak := peakOver(spans, from, cand.to)
	if !quota.Fits(peak, tenant.Totals{}) {
		return refusal(client.ReasonOverQuota, "the tenant's reservations would reserve up to cpu %dm and memory %d bytes at once, over its quota%s",
			peak.CPU, peak.Memory, quotaText(quota))
	}
	return nil
}

func quotaText(q tenant.Limits) string {
	s := ""
	if q.HasCPU {
		s += fmt.Sprintf(" cpu %dm", q.CPU)
	}
	if q.HasMemory {
		s += fmt.Sprintf(" memory %d bytes", q.Memory)
	}
	return s
}

// peakOver is the most the spans reserve at the same time within [from, to), per resource (the two peaks may fall at
// different times). The sum only changes where a span starts, so those instants (and from) are the ones to look at.
func peakOver(spans []reservationWindow, from, to time.Time) tenant.Totals {
	points := []time.Time{from}
	for _, s := range spans {
		if s.from.After(from) && s.from.Before(to) {
			points = append(points, s.from)
		}
	}
	var peak tenant.Totals
	for _, p := range points {
		var sum tenant.Totals
		for _, s := range spans {
			if !s.from.After(p) && s.to.After(p) {
				sum.CPU, sum.Memory = sum.CPU+s.cpu, sum.Memory+s.mem
			}
		}
		peak.CPU, peak.Memory = max(peak.CPU, sum.CPU), max(peak.Memory, sum.Memory)
	}
	return peak
}

// CreateReservations creates the tenant's reservations. Re-sending one with the same window and amount is EXISTS (labels it
// lacks are added: UPDATED); another window or amount under the same id is FAILED. A window that is not valid (see
// validateReservationWindow) is FAILED for its item; an id or label that is not is INVALID_ARGUMENT for the call.
func (h *Handler) CreateReservations(ctx context.Context, in *protobuf.CreateReservationsRequest) (*protobuf.BatchResult, error) {
	items := in.GetItems()
	if err := checkItemCount(len(items)); err != nil {
		return nil, err
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, invalid("%v", err)
	}
	refs := make([]*protobuf.ItemRef, len(items))
	for i, it := range items {
		refs[i] = reservationRef(it.GetId())
		if err := names.ValidateID(it.GetId()); err != nil {
			return nil, invalid("item %d: %v", i, err)
		}
		if err := validateLabels(it.GetLabels()); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetId(), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	t := tenantOf(ctx)
	// The quota check reads the tenant's reservations: the writes of one tenant go one at a time in this process.
	defer h.keyLock("tenant/" + t)()
	quota, err := h.reservationQuota(ctx, t)
	if err != nil {
		return nil, err
	}
	have, err := h.listReservations(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]*protobuf.ItemResult, len(items))
	for i, it := range items {
		state, created, err := h.createReservation(ctx, it, in.GetLabels(), t, quota, have)
		if err != nil {
			out[i] = failedResult(refs[i], err)
			continue
		}
		if created != nil {
			have = append(have, *created)
		}
		out[i] = result(refs[i], state)
	}
	return &protobuf.BatchResult{Results: out}, nil
}

// createReservation creates one reservation, or answers for the one that exists under its id. created is the new object.
func (h *Handler) createReservation(ctx context.Context, it *protobuf.ReservationItem, common map[string]string, t string, quota tenant.Limits,
	have []laboratoryv1alpha1.Reservation) (state protobuf.ItemState, created *laboratoryv1alpha1.Reservation, err error) {
	now := h.now()
	w := itemWindow(it)
	if err := validateReservationWindow(w, now); err != nil {
		return 0, nil, err
	}
	name := crName(it.GetId())
	want := mergeItemLabels(common, it.GetLabels())
	for i := range have {
		if have[i].Name == name {
			state, err := h.existingReservation(ctx, it.GetId(), w, want)
			return state, nil, err
		}
	}
	if err := tenantPeakFits(quota, have, name, w, now); err != nil {
		return 0, nil, err
	}
	r := &laboratoryv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: name}}
	r.Spec.Tenant = t
	w.apply(&r.Spec)
	r.Labels, r.Annotations = stampTenant(copyLabels(want), t), stampID(it.GetId())
	reservations := h.cs.LaboratoryV1alpha1().Reservations()
	out, err := reservations.Create(ctx, r, metav1.CreateOptions{})
	if err == nil {
		return protobuf.ItemState_ITEM_STATE_CREATED, out, nil
	}
	if err = createErr(err, kindReservation, it.GetId(), func() (metav1.Object, error) {
		cur, err := reservations.Get(ctx, name, metav1.GetOptions{})
		if err == nil && !ownsReservation(t, cur) {
			return nil, errTaken{kindReservation, it.GetId()}
		}
		return cur, err
	}); !apierrors.IsAlreadyExists(err) {
		return 0, nil, err
	}
	// made in between (another replica): the same answer as for one that was listed
	state, err = h.existingReservation(ctx, it.GetId(), w, want)
	return state, nil, err
}

// existingReservation answers a create of an id that exists: the same window and amount is EXISTS (labels it lacks are added:
// UPDATED), another is FAILED; another tenant's is taken, one being deleted is retryable.
func (h *Handler) existingReservation(ctx context.Context, id string, w reservationWindow, want map[string]string) (protobuf.ItemState, error) {
	reservations := h.cs.LaboratoryV1alpha1().Reservations()
	state := protobuf.ItemState_ITEM_STATE_EXISTS
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		cur, err := reservations.Get(ctx, crName(id), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !ownsReservation(tenantOf(ctx), cur) {
			return errTaken{kindReservation, id}
		}
		if err := rejectTerminating(kindReservation, cur); err != nil {
			return err
		}
		if names.IDOf(cur) != id {
			return fmt.Errorf("%s %s: the name is taken by another id %q", kindReservation, id, names.IDOf(cur))
		}
		if windowOf(cur) != w {
			return errDifferentSpec{kindReservation, id}
		}
		labels, changed := mergeLabels(cur.Labels, want)
		if !changed {
			return nil
		}
		cur.Labels = labels
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		_, err = reservations.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	return state, err
}

// ListReservations lists the tenant's reservations by ids (items), by selector, or all of them.
func (h *Handler) ListReservations(ctx context.Context, in *protobuf.ListRequest) (*protobuf.ReservationList, error) {
	if err := checkListRequest(in); err != nil {
		return nil, err
	}
	out := &protobuf.ReservationList{}
	notice := h.reservationNotice()
	if len(in.GetItems()) > 0 {
		for _, ref := range in.GetItems() {
			r, err := h.getReservation(ctx, ref.GetName())
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out.Items = append(out.Items, reservationToProto(r, notice))
		}
		return out, nil
	}
	items, err := h.listReservations(ctx, in.GetSelector())
	if err != nil {
		return nil, err
	}
	for i := range items {
		out.Items = append(out.Items, reservationToProto(&items[i], notice))
	}
	return out, nil
}

// reservationPlan is a validated set of changes for one reservation.
type reservationPlan struct {
	labels             labelPlan
	from, to, cpu, mem *int64
}

func planReservationChanges(common, item *protobuf.ReservationChanges) (reservationPlan, error) {
	var p reservationPlan
	var err error
	if p.labels, err = mergeLabelChanges(common.GetLabels(), item.GetLabels()); err != nil {
		return p, err
	}
	if item == nil {
		item = &protobuf.ReservationChanges{}
	}
	if common == nil {
		common = &protobuf.ReservationChanges{}
	}
	pick := func(c, i *int64) *int64 {
		if i != nil {
			return i
		}
		return c
	}
	p.from, p.to = pick(common.FromUnixMs, item.FromUnixMs), pick(common.ToUnixMs, item.ToUnixMs)
	p.cpu, p.mem = pick(common.CpuMillicores, item.CpuMillicores), pick(common.MemoryBytes, item.MemoryBytes)
	return p, nil
}

// UpdateReservations moves the window and changes the amount and labels of reservations; the id stays. from changes only
// while the reservation has not started, to must stay in the future (DeleteReservations ends one now). A window that grows or
// an amount that rises is checked against the tenant's quota again; a smaller amount is allowed (the operator decides about
// what runs under it: the Exceeded notice).
func (h *Handler) UpdateReservations(ctx context.Context, in *protobuf.UpdateReservationsRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	t := tenantOf(ctx)
	defer h.keyLock("tenant/" + t)()
	have, err := h.listReservations(ctx, "")
	if err != nil {
		return nil, err
	}
	var refs []*protobuf.ItemRef
	var plans []reservationPlan
	if in.GetBySelector() != nil {
		items, err := h.listReservations(ctx, in.GetBySelector().GetSelector())
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(items), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		plan, err := planReservationChanges(in.GetChanges(), nil)
		if err != nil {
			return nil, invalid("%v", err)
		}
		for i := range items {
			refs, plans = append(refs, reservationRef(names.IDOf(&items[i]))), append(plans, plan)
		}
		sortRefs(refs)
	} else {
		for _, it := range in.GetItems() {
			ref := reservationRef(it.GetId())
			plan, err := planReservationChanges(in.GetChanges(), it.GetChanges())
			if err != nil {
				return nil, invalid("%s: %v", it.GetId(), err)
			}
			refs, plans = append(refs, ref), append(plans, plan)
		}
		if err := dupRefs(refs); err != nil {
			return nil, err
		}
	}
	quota, err := h.reservationQuota(ctx, t)
	if err != nil {
		return nil, err
	}
	out := make([]*protobuf.ItemResult, len(refs))
	for i, ref := range refs {
		updated, err := h.updateReservation(ctx, ref.GetName(), plans[i], quota, have)
		if err != nil {
			out[i] = failedResult(ref, err)
			continue
		}
		for j := range have {
			if have[j].Name == updated.Name {
				have[j] = *updated
			}
		}
		out[i] = result(ref, protobuf.ItemState_ITEM_STATE_UPDATED)
	}
	return &protobuf.BatchResult{Results: out}, nil
}

func (h *Handler) updateReservation(ctx context.Context, id string, p reservationPlan, quota tenant.Limits, have []laboratoryv1alpha1.Reservation) (*laboratoryv1alpha1.Reservation, error) {
	reservations := h.cs.LaboratoryV1alpha1().Reservations()
	var out *laboratoryv1alpha1.Reservation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := h.getReservation(ctx, id)
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindReservation, cur); err != nil {
			return err
		}
		now := h.now()
		old := windowOf(cur)
		w := old
		if p.from != nil && !unixMs(*p.from).Equal(old.from) {
			if !now.Before(old.from) {
				return fmt.Errorf("%s %s has started: from cannot change", kindReservation, id)
			}
			w.from = unixMs(*p.from)
		}
		if p.to != nil {
			w.to = unixMs(*p.to)
		}
		if p.cpu != nil {
			w.cpu = *p.cpu
		}
		if p.mem != nil {
			w.mem = *p.mem
		}
		if w != old {
			if err := validateReservationWindow(w, now); err != nil {
				return err
			}
		}
		if w.cpu > old.cpu || w.mem > old.mem || w.from.Before(old.from) || w.to.After(old.to) {
			if err := tenantPeakFits(quota, have, cur.Name, w, now); err != nil {
				return err
			}
		}
		labels, labelsChanged := p.labels.apply(cur.Labels)
		if w == old && !labelsChanged {
			out = cur
			return nil
		}
		cur.Labels = labels
		w.apply(&cur.Spec)
		out, err = reservations.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	return out, err
}

// DeleteReservations deletes reservations. The groups that ran under one get the ending notice and are deleted after it (the
// operator's orphan path); to end a window with its groups at a later time, move `to` instead.
func (h *Handler) DeleteReservations(ctx context.Context, in *protobuf.DeleteRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var refs []*protobuf.ItemRef
	if in.GetBySelector() != nil {
		items, err := h.listReservations(ctx, in.GetBySelector().GetSelector())
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(items), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for i := range items {
			refs = append(refs, reservationRef(names.IDOf(&items[i])))
		}
		sortRefs(refs)
	} else {
		for _, r := range in.GetItems() {
			refs = append(refs, reservationRef(r.GetName()))
		}
		if err := dupRefs(refs); err != nil {
			return nil, err
		}
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		r, err := h.getReservation(ctx, refs[i].GetName())
		if err != nil {
			return failedResult(refs[i], err)
		}
		uid := r.UID
		return deleteResult(refs[i], h.cs.LaboratoryV1alpha1().Reservations().Delete(ctx, r.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
	})}, nil
}

// reservationToProto is the API view of a reservation. notice is the ending notice of the cluster.
func reservationToProto(r *laboratoryv1alpha1.Reservation, notice time.Duration) *protobuf.Reservation {
	w := windowOf(r)
	return &protobuf.Reservation{
		Id: names.IDOf(r), FromUnixMs: w.from.UnixMilli(), ToUnixMs: w.to.UnixMilli(),
		CpuMillicores: w.cpu, MemoryBytes: w.mem,
		Labels: userLabels(r.Labels), Status: reservationStatusToProto(r, notice), Uid: string(r.UID),
	}
}

var reservationPhases = map[laboratoryv1alpha1.ReservationPhase]protobuf.ReservationPhase{
	laboratoryv1alpha1.ReservationPhasePending: protobuf.ReservationPhase_RESERVATION_PHASE_PENDING,
	laboratoryv1alpha1.ReservationPhaseActive:  protobuf.ReservationPhase_RESERVATION_PHASE_ACTIVE,
	laboratoryv1alpha1.ReservationPhaseEnding:  protobuf.ReservationPhase_RESERVATION_PHASE_ENDING,
	laboratoryv1alpha1.ReservationPhaseEnded:   protobuf.ReservationPhase_RESERVATION_PHASE_ENDED,
}

// reservationStatusToProto is what the operator observed, with the end of the groups when one is announced:
//   - Ending for the window (WindowClosing): at `to`;
//   - Ending because the tenant is gone (TenantGone): one notice after it was given (at least a minute, as the operator),
//     or `to` when that is sooner;
//   - Exceeded (AmountReduced, the phase may stay Active): the groups created before the shrink go one notice after it.
//
// When two apply, the sooner wins.
func reservationStatusToProto(r *laboratoryv1alpha1.Reservation, notice time.Duration) *protobuf.ReservationStatus {
	st := &r.Status
	out := &protobuf.ReservationStatus{
		Phase:             reservationPhases[st.Phase],
		UsedCpuMillicores: st.Used.Cpu().MilliValue(), UsedMemoryBytes: st.Used.Memory().Value(),
		Groups: st.Groups,
	}
	if st.UsedAt != nil {
		out.UsedAtUnixMs = st.UsedAt.UnixMilli()
	}
	if st.NoticeAt != nil {
		out.NoticeAtUnixMs = st.NoticeAt.UnixMilli()
	}
	isTrue := func(t string) bool { return meta.IsStatusConditionTrue(st.Conditions, t) }
	out.OverAmount, out.Invalid = isTrue(laboratoryv1alpha1.ReservationConditionOverAmount), isTrue(laboratoryv1alpha1.ReservationConditionInvalid)
	loss := max(notice, reservationLossFloor)
	var end time.Time
	var reason string
	if st.Phase == laboratoryv1alpha1.ReservationPhaseEnding {
		end, reason = r.Spec.To.Time, laboratoryv1alpha1.ReservationReasonWindowClosing
		if c := meta.FindStatusCondition(st.Conditions, laboratoryv1alpha1.ReservationConditionEnding); c != nil && c.Reason == laboratoryv1alpha1.ReservationReasonTenantGone {
			reason = laboratoryv1alpha1.ReservationReasonTenantGone
			if st.NoticeAt != nil && st.NoticeAt.Add(loss).Before(end) {
				end = st.NoticeAt.Add(loss)
			}
		}
	}
	if c := meta.FindStatusCondition(st.Conditions, laboratoryv1alpha1.ReservationConditionExceeded); c != nil && c.Status == metav1.ConditionTrue &&
		c.Reason == laboratoryv1alpha1.ReservationReasonAmountReduced && st.Phase != laboratoryv1alpha1.ReservationPhaseEnded {
		out.Exceeded = true
		if at := c.LastTransitionTime.Add(loss); end.IsZero() || at.Before(end) {
			end, reason = at, laboratoryv1alpha1.ReservationReasonAmountReduced
		}
	}
	if !end.IsZero() {
		out.EndsAtUnixMs, out.EndingReason = end.UnixMilli(), reason
	}
	return out
}

// reservationIDOf is the id of the reservation a group or lab runs under, from its label (the CR name): the reservation's
// original id when it is known, else the label itself; "" for none.
func reservationIDOf(ids map[string]string, label string) string {
	if id := ids[label]; id != "" {
		return id
	}
	return label
}

// reservationIDs maps the CR names of the caller's tenant's reservations to their ids.
func (h *Handler) reservationIDs(ctx context.Context) (map[string]string, error) {
	list, err := h.listReservations(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for i := range list {
		out[list[i].Name] = names.IDOf(&list[i])
	}
	return out, nil
}
