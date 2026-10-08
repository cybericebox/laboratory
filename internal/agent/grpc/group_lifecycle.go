package grpc

import (
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	controller "github.com/cybericebox/laboratory/internal/controller/laboratory"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	equality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"strings"
	"unicode/utf8"
)

// StopLabGroups accepts intent; it never force-stops children or reports release.
func (h *Handler) StopLabGroups(ctx context.Context, in *protobuf.StopLabGroupsRequest) (*protobuf.BatchResult, error) {
	if err := checkLifecycleCount(len(in.GetItems())); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(in.GetItems()))
	for i, it := range in.GetItems() {
		refs[i] = &protobuf.ItemRef{Name: it.GetTarget().GetGroup()}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		it := in.Items[i]
		if !it.GetRequireAllLabsStopped() {
			return failedResult(refs[i], fmt.Errorf("require_all_labs_stopped must be true"))
		}
		t := it.GetTarget()
		intent := &lab.GroupLifecycleSpec{DesiredState: "Stopped", OperationID: t.GetOperationId(), Revision: t.GetRevision(), RequireAllLabsStopped: true}
		if err := h.acceptGroupLifecycle(ctx, t, intent); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}

// StartLabGroups starts group services only. Every stopped/terminal Lab is unchanged.
func (h *Handler) StartLabGroups(ctx context.Context, in *protobuf.StartLabGroupsRequest) (*protobuf.BatchResult, error) {
	if err := checkLifecycleCount(len(in.GetItems())); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(in.GetItems()))
	for i, t := range in.GetItems() {
		refs[i] = &protobuf.ItemRef{Name: t.GetGroup()}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		t := in.Items[i]
		intent := &lab.GroupLifecycleSpec{DesiredState: "Running", OperationID: t.GetOperationId(), Revision: t.GetRevision()}
		if err := h.acceptGroupLifecycle(ctx, t, intent); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}
func (h *Handler) acceptGroupLifecycle(ctx context.Context, t *protobuf.GroupTarget, intent *lab.GroupLifecycleSpec) error {
	h.lifecycleAdmissionMu.Lock()
	defer h.lifecycleAdmissionMu.Unlock()
	if err := names.ValidateID(t.GetGroup()); err != nil {
		return err
	}
	if t.GetExpectedUid() == "" || strings.TrimSpace(intent.OperationID) == "" || utf8.RuneCountInString(intent.OperationID) > 128 || intent.Revision < 1 {
		return fmt.Errorf("expected_uid, operation_id (1..128 characters) and positive revision are required")
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := h.getGroup(ctx, t.GetGroup())
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroup, cur); err != nil {
			return err
		}
		if string(cur.UID) != t.GetExpectedUid() {
			return fmt.Errorf("group UID differs from expected_uid")
		}
		if old := cur.Spec.Lifecycle; old != nil {
			if intent.Revision < old.Revision {
				return fmt.Errorf("group lifecycle revision is stale")
			}
			if intent.Revision == old.Revision {
				if equality.Semantic.DeepEqual(old, intent) {
					return nil
				}
				return fmt.Errorf("conflicting group intent at equal revision")
			}
		}
		if intent.IsStopped() && cur.Spec.Admission != nil {
			pending := cur.Spec.Admission
			if !h.childAdmissionWritten(ctx, cur, pending) {
				return pendingAdmissionError{group: t.GetGroup(), token: pending.Token}
			}
			cur.Spec.Admission = nil // exact childwrite observed; same resourceVersion CAS as stop
		}
		if intent.IsStopped() {
			validator := controller.LabGroupReconciler{Reader: snapshotAcceptanceReader{h.cs}}
			if err := validator.ValidateGroupStop(ctx, cur); err != nil {
				return err
			}
		}
		cur.Spec.Lifecycle = intent.DeepCopy()
		_, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}
