package grpc

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const kindLabGroup = "LabGroup"

// CreateLabGroup creates a new LabGroup custom resource.
func (h *Handler) CreateLabGroup(ctx context.Context, in *protobuf.LabGroup) (*protobuf.LabGroup, error) {
	lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Labels: copyLabels(in.Labels)}}
	out, err := h.cs.LaboratoryV1alpha1().LabGroups().Create(ctx, lg, metav1.CreateOptions{})
	if err := createErr(err, kindLabGroup, in.Name, func() (metav1.Object, error) {
		return h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, in.Name, metav1.GetOptions{})
	}); err != nil {
		return nil, err
	}
	return labGroupToProto(out), nil
}

// GetLabGroup fetches a LabGroup custom resource by name.
func (h *Handler) GetLabGroup(ctx context.Context, in *protobuf.IDRequest) (*protobuf.LabGroup, error) {
	out, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return labGroupToProto(out), nil
}

// ListLabGroups lists all LabGroup custom resources.
func (h *Handler) ListLabGroups(ctx context.Context, _ *protobuf.Empty) (*protobuf.LabGroupList, error) {
	list, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := &protobuf.LabGroupList{}
	for i := range list.Items {
		out.Items = append(out.Items, labGroupToProto(&list.Items[i]))
	}
	return out, nil
}

// UpdateLabGroup merges the given labels into the LabGroup and returns it. LabGroup has
// no other user-mutable fields, so without labels this is a re-fetch.
func (h *Handler) UpdateLabGroup(ctx context.Context, in *protobuf.LabGroup) (*protobuf.LabGroup, error) {
	cur, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := rejectTerminating(kindLabGroup, cur); err != nil {
		return nil, err
	}
	if labels, changed := mergeLabels(cur.Labels, in.Labels); changed {
		cur.Labels = labels
		var err error
		if cur, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
	}
	return labGroupToProto(cur), nil
}

// SetLabGroupSuspended updates only the LabGroup's desired runtime state. The
// operator performs the non-destructive workload scale-down or resume.
func (h *Handler) SetLabGroupSuspended(ctx context.Context, in *protobuf.LabGroupSuspendRequest) (*protobuf.LabGroup, error) {
	group, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := rejectTerminating(kindLabGroup, group); err != nil {
		return nil, err
	}
	if group.Spec.Suspended == in.Suspended {
		return labGroupToProto(group), nil
	}
	group.Spec.Suspended = in.Suspended
	group, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, group, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return labGroupToProto(group), nil
}

// SetLabGroupVPNDisabled changes only the VPN deployment's desired state.
// Labs continue to follow the independent group suspension flag; the internet
// gateway stays running until the group is deleted.
func (h *Handler) SetLabGroupVPNDisabled(ctx context.Context, in *protobuf.LabGroupVPNDisabledRequest) (*protobuf.LabGroup, error) {
	group, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := rejectTerminating(kindLabGroup, group); err != nil {
		return nil, err
	}
	if group.Spec.VPN.Disabled == in.Disabled && group.Spec.VPN.ProbeWhileSuspended == in.ProbeWhileSuspended {
		return labGroupToProto(group), nil
	}
	group.Spec.VPN.Disabled = in.Disabled
	group.Spec.VPN.ProbeWhileSuspended = in.ProbeWhileSuspended
	group, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, group, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return labGroupToProto(group), nil
}

// DeleteLabGroup deletes a LabGroup custom resource by name. Deletion is
// asynchronous (finalizers): it is finished once GetLabGroup returns NotFound.
// Until then CreateLabGroup and the other writes on that name fail with a
// retryable Unavailable / TERMINATING error.
func (h *Handler) DeleteLabGroup(ctx context.Context, in *protobuf.IDRequest) (*protobuf.Empty, error) {
	if err := h.cs.LaboratoryV1alpha1().LabGroups().Delete(ctx, in.Name, metav1.DeleteOptions{}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}
