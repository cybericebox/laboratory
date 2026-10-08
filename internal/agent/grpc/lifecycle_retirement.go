package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	controller "github.com/cybericebox/laboratory/internal/controller/laboratory"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"strings"
	"unicode/utf8"
)

func retirementIntent(uid, stopOp string, stopRev, generation int64, op string, rev int64) (lab.LifecycleRetirementIntent, error) {
	if uid == "" || stopOp == "" || stopRev < 1 || generation < 1 || strings.TrimSpace(op) == "" || utf8.RuneCountInString(op) > 128 || rev <= stopRev || op == stopOp {
		return lab.LifecycleRetirementIntent{}, fmt.Errorf("retirement requires original stop identity and a distinct operation with a newer revision")
	}
	return lab.LifecycleRetirementIntent{ExpectedUID: uid, StopOperationID: stopOp, StopRevision: stopRev, Generation: generation, OperationID: op, Revision: rev, RequestedAt: metav1.Now()}, nil
}
func claimRetirement(annotations map[string]string, in lab.LifecycleRetirementIntent) (map[string]string, bool, error) {
	if raw := annotations[names.AnnotationLifecycleRetirement]; raw != "" {
		old, valid := lab.ParseLifecycleRetirement(raw)
		if !valid || old.ExpectedUID != in.ExpectedUID || old.StopOperationID != in.StopOperationID || old.StopRevision != in.StopRevision || old.OperationID != in.OperationID || old.Revision != in.Revision {
			return nil, false, fmt.Errorf("retirement is already fenced to another identity")
		}
		return annotations, false, nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, false, err
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[names.AnnotationLifecycleRetirement] = string(raw)
	return annotations, true, nil
}
func (h *Handler) RetireLabs(ctx context.Context, in *protobuf.RetireLabsRequest) (*protobuf.BatchResult, error) {
	if err := checkLifecycleCount(len(in.GetItems())); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(in.GetItems()))
	for i, target := range in.GetItems() {
		refs[i] = target.GetStopTarget().GetRef()
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		item := in.Items[i]
		target := item.GetStopTarget()
		ref := target.GetRef()
		if ref.GetLabGroup() == "" || ref.GetName() == "" || ref.GetLab() != "" {
			return failedResult(ref, fmt.Errorf("stop target requires lab_group and name"))
		}
		h.lifecycleAdmissionMu.Lock()
		defer h.lifecycleAdmissionMu.Unlock()
		ns, err := resolver.namespace(ctx, ref.GetLabGroup())
		if err != nil {
			return failedResult(ref, err)
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := h.cs.LaboratoryV1alpha1().Labs(ns).Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
			if err != nil {
				return err
			}
			if !ownedBy(tenantOf(ctx), cur) {
				return notFoundForeign(kindLab, ref.GetName())
			}
			if err = rejectTerminating(kindLab, cur); err != nil {
				return err
			}
			requested, err := retirementIntent(target.GetExpectedLabUid(), target.GetOperationId(), target.GetLifecycleRevision(), cur.Generation, item.GetRetirementOperationId(), item.GetRetirementRevision())
			if err != nil {
				return err
			}
			if string(cur.UID) != requested.ExpectedUID || cur.Spec.Lifecycle == nil || cur.Spec.Lifecycle.OperationID != requested.StopOperationID || cur.Spec.Lifecycle.Revision != requested.StopRevision {
				return fmt.Errorf("original stopped Lab identity changed")
			}
			annotations, changed, err := claimRetirement(cur.Annotations, requested)
			if err != nil || !changed {
				return err
			}
			if cur.Annotations[names.AnnotationLabVariableAdmission] != "" {
				return fmt.Errorf("Lab variable write is pending; replay it before retirement")
			}
			if !controller.RetirementReady(cur) {
				return fmt.Errorf("Lab must be exactly Stopped and native Released before retirement")
			}
			cur.Annotations = annotations
			_, err = h.cs.LaboratoryV1alpha1().Labs(ns).Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			return failedResult(ref, err)
		}
		return result(ref, protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}
func (h *Handler) RetireLabGroups(ctx context.Context, in *protobuf.RetireLabGroupsRequest) (*protobuf.BatchResult, error) {
	if err := checkLifecycleCount(len(in.GetItems())); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(in.GetItems()))
	for i, item := range in.Items {
		refs[i] = groupRef(item.GetStopTarget().GetGroup())
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		item := in.Items[i]
		target := item.GetStopTarget()
		h.lifecycleAdmissionMu.Lock()
		defer h.lifecycleAdmissionMu.Unlock()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := h.getGroup(ctx, target.GetGroup())
			if err != nil {
				return err
			}
			if err = rejectTerminating(kindLabGroup, cur); err != nil {
				return err
			}
			requested, err := retirementIntent(target.GetExpectedUid(), target.GetOperationId(), target.GetRevision(), cur.Generation, item.GetRetirementOperationId(), item.GetRetirementRevision())
			if err != nil {
				return err
			}
			if string(cur.UID) != requested.ExpectedUID || cur.Spec.Lifecycle == nil || cur.Spec.Lifecycle.OperationID != requested.StopOperationID || cur.Spec.Lifecycle.Revision != requested.StopRevision {
				return fmt.Errorf("original stopped Group identity changed")
			}
			ns, err := h.k8s.CoreV1().Namespaces().Get(ctx, lab.LabGroupNamespaceOf(cur), metav1.GetOptions{})
			if err != nil {
				return err
			}
			if ns.Labels[names.LabelGroup] != cur.Name || ns.UID == "" {
				return fmt.Errorf("group namespace ownership changed")
			}
			requested.NamespaceUID = string(ns.UID)
			annotations, changed, err := claimRetirement(cur.Annotations, requested)
			if err != nil || !changed {
				return err
			}
			if !controller.GroupRetirementReady(cur) {
				return fmt.Errorf("Group services must be exactly Stopped and native Released before retirement")
			}
			validator := controller.LabGroupReconciler{Reader: snapshotAcceptanceReader{h.cs}}
			if err := validator.ValidateGroupStop(ctx, cur); err != nil {
				return err
			}
			children, err := h.cs.LaboratoryV1alpha1().Labs(lab.LabGroupNamespaceOf(cur)).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			for n := range children.Items {
				if !controller.LifecycleRetired(&children.Items[n]) {
					return fmt.Errorf("all child Labs must have confirmed retirement")
				}
			}
			cur.Annotations = annotations
			_, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}
