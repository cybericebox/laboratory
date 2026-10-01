package grpc

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const kindLabGroup = "LabGroup"

func groupRef(name string) *protobuf.ItemRef { return &protobuf.ItemRef{Name: name} }

// CreateLabGroups creates LabGroup custom resources. Re-sending an item whose group
// exists with the same spec is EXISTS (labels it lacks are added: UPDATED); a group with
// a different spec is FAILED for that item.
func (h *Handler) CreateLabGroups(ctx context.Context, in *protobuf.CreateLabGroupsRequest) (*protobuf.BatchResult, error) {
	items := in.GetItems()
	if err := checkItemCount(len(items)); err != nil {
		return nil, err
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, invalid("%v", err)
	}
	refs := make([]*protobuf.ItemRef, len(items))
	for i, it := range items {
		refs[i] = groupRef(it.GetName())
		if err := names.ValidateID(it.GetName()); err != nil {
			return nil, invalid("item %d: %v", i, err)
		}
		if err := validateLabels(it.GetLabels()); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
		if _, err := newDeploySpec(it.GetDeployGroup(), it.GetDeployAfter()); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		return h.createLabGroup(ctx, items[i], in.GetLabels())
	})}, nil
}

func (h *Handler) createLabGroup(ctx context.Context, it *protobuf.LabGroupItem, common map[string]string) *protobuf.ItemResult {
	ref := groupRef(it.GetName())
	name := crName(it.GetName())
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	want := mergeItemLabels(common, it.GetLabels())
	dep, _ := newDeploySpec(it.GetDeployGroup(), it.GetDeployAfter())
	lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
	lg.Spec.Suspended = it.GetSuspended()
	lg.Spec.VPN.Disabled = it.GetVpnDisabled()
	lg.Spec.VPN.ProbeWhileSuspended = it.GetProbeWhileSuspended()
	lg.Labels, lg.Annotations = dep.stamp(copyLabels(want), stampID(nil, it.GetName()))

	_, err := groups.Create(ctx, lg, metav1.CreateOptions{})
	if err == nil {
		return result(ref, protobuf.ItemState_ITEM_STATE_CREATED)
	}
	if err = createErr(err, kindLabGroup, it.GetName(), func() (metav1.Object, error) {
		return groups.Get(ctx, name, metav1.GetOptions{})
	}); !apierrors.IsAlreadyExists(err) {
		return failedResult(ref, err)
	}

	state := protobuf.ItemState_ITEM_STATE_EXISTS
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		cur, err := groups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroup, cur); err != nil {
			return err
		}
		if names.IDOf(cur) != it.GetName() {
			return fmt.Errorf("%s %s: the name is taken by another id %q", kindLabGroup, it.GetName(), names.IDOf(cur))
		}
		if cur.Spec.Suspended != lg.Spec.Suspended || cur.Spec.VPN.Disabled != lg.Spec.VPN.Disabled ||
			cur.Spec.VPN.ProbeWhileSuspended != lg.Spec.VPN.ProbeWhileSuspended || !dep.matches(cur.Labels, cur.Annotations) {
			return errDifferentSpec{kindLabGroup, it.GetName()}
		}
		labels, changed := mergeLabels(cur.Labels, want)
		if !changed {
			return nil
		}
		cur.Labels = labels
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		_, err = groups.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return failedResult(ref, err)
	}
	return result(ref, state)
}

// errDifferentSpec is an existing object whose spec differs from the one sent.
type errDifferentSpec struct{ kind, name string }

func (e errDifferentSpec) Error() string {
	return fmt.Sprintf("%s %s already exists with a different spec", e.kind, e.name)
}

// ListLabGroups lists LabGroups by names (items), by selector, or all of them.
func (h *Handler) ListLabGroups(ctx context.Context, in *protobuf.ListRequest) (*protobuf.LabGroupList, error) {
	if err := checkListRequest(in); err != nil {
		return nil, err
	}
	out := &protobuf.LabGroupList{}
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	if len(in.GetItems()) > 0 {
		for _, ref := range in.GetItems() {
			g, err := groups.Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out.Items = append(out.Items, labGroupToProto(g))
		}
		return out, nil
	}
	list, err := groups.List(ctx, metav1.ListOptions{LabelSelector: in.GetSelector()})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		out.Items = append(out.Items, labGroupToProto(&list.Items[i]))
	}
	return out, nil
}

// checkListRequest: items or a selector, never both; items must be non-empty refs.
func checkListRequest(in *protobuf.ListRequest) error {
	if in.GetSelector() != "" && len(in.GetItems()) > 0 {
		return invalid("give a selector or items, not both")
	}
	if err := parseSelector(in.GetSelector(), true); err != nil {
		return err
	}
	return checkItemCount(len(in.GetItems()))
}

type groupTarget struct {
	ref     *protobuf.ItemRef
	changes *protobuf.LabGroupChanges
}

// UpdateLabGroups changes labels and the mutable spec fields of the targeted groups.
func (h *Handler) UpdateLabGroups(ctx context.Context, in *protobuf.UpdateLabGroupsRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var targets []groupTarget
	if in.GetBySelector() != nil {
		list, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{LabelSelector: in.GetBySelector().GetSelector()})
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(list.Items), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for i := range list.Items {
			targets = append(targets, groupTarget{ref: groupRef(names.IDOf(&list.Items[i]))})
		}
		sort.Slice(targets, func(i, j int) bool { return refKey(targets[i].ref) < refKey(targets[j].ref) })
	} else {
		for _, it := range in.GetItems() {
			targets = append(targets, groupTarget{ref: groupRef(it.GetName()), changes: it.GetChanges()})
		}
	}
	plans := make([]groupPlan, len(targets))
	refs := make([]*protobuf.ItemRef, len(targets))
	for i, t := range targets {
		refs[i] = t.ref
		p, err := planGroupChanges(in.GetChanges(), t.changes)
		if err != nil {
			return nil, invalid("%s: %v", describeRef(t.ref), err)
		}
		plans[i] = p
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		return h.updateLabGroup(ctx, refs[i], plans[i])
	})}, nil
}

// groupPlan is a validated set of changes for one group.
type groupPlan struct {
	labels                                 labelPlan
	suspended, vpnDisabled, probeWhileSusp *bool
}

// planGroupChanges overlays the item's changes (nil in a selector call) on the
// request-level ones.
func planGroupChanges(common, item *protobuf.LabGroupChanges) (groupPlan, error) {
	var p groupPlan
	var err error
	if p.labels, err = mergeLabelChanges(common.GetLabels(), item.GetLabels()); err != nil {
		return p, err
	}
	pick := func(c, i *bool) *bool {
		if i != nil {
			return i
		}
		return c
	}
	if item == nil {
		item = &protobuf.LabGroupChanges{}
	}
	if common == nil {
		common = &protobuf.LabGroupChanges{}
	}
	p.suspended = pick(common.Suspended, item.Suspended)
	p.vpnDisabled = pick(common.VpnDisabled, item.VpnDisabled)
	p.probeWhileSusp = pick(common.ProbeWhileSuspended, item.ProbeWhileSuspended)
	return p, nil
}

func (h *Handler) updateLabGroup(ctx context.Context, ref *protobuf.ItemRef, p groupPlan) *protobuf.ItemResult {
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := groups.Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroup, cur); err != nil {
			return err
		}
		changed := false
		var c bool
		if cur.Labels, c = p.labels.apply(cur.Labels); c {
			changed = true
		}
		set := func(dst *bool, v *bool) {
			if v != nil && *dst != *v {
				*dst = *v
				changed = true
			}
		}
		set(&cur.Spec.Suspended, p.suspended)
		set(&cur.Spec.VPN.Disabled, p.vpnDisabled)
		set(&cur.Spec.VPN.ProbeWhileSuspended, p.probeWhileSusp)
		if !changed {
			return nil
		}
		_, err = groups.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return failedResult(ref, err)
	}
	return result(ref, protobuf.ItemState_ITEM_STATE_UPDATED)
}

// DeleteLabGroups deletes LabGroups. Deletion is asynchronous (finalizers): it is
// finished once ListLabGroups no longer returns the group. Until then Create and the
// other writes on that name fail with a retryable TERMINATING error.
func (h *Handler) DeleteLabGroups(ctx context.Context, in *protobuf.DeleteRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var refs []*protobuf.ItemRef
	if in.GetBySelector() != nil {
		list, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{LabelSelector: in.GetBySelector().GetSelector()})
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(list.Items), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for i := range list.Items {
			refs = append(refs, groupRef(names.IDOf(&list.Items[i])))
		}
		sortRefs(refs)
	} else {
		for _, r := range in.GetItems() {
			refs = append(refs, groupRef(r.GetName()))
		}
		if err := dupRefs(refs); err != nil {
			return nil, err
		}
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		return deleteResult(refs[i], h.cs.LaboratoryV1alpha1().LabGroups().Delete(ctx, crName(refs[i].GetName()), metav1.DeleteOptions{}))
	})}, nil
}

// deleteResult maps the error of a Delete call.
func deleteResult(ref *protobuf.ItemRef, err error) *protobuf.ItemResult {
	if err != nil {
		return failedResult(ref, err)
	}
	return result(ref, protobuf.ItemState_ITEM_STATE_DELETED)
}
