package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	equality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func admissionToken(a *lab.GroupChildAdmission) string {
	copy := *a
	copy.Token = ""
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// claimChildAdmission CASes the same Group resource as StopLabGroups. A stopped
// group cannot gain an admission, and a pending admission prevents group stop.
func (h *Handler) claimChildAdmission(ctx context.Context, group string, a *lab.GroupChildAdmission) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		g, err := h.getGroup(ctx, group)
		if err != nil {
			return err
		}
		if g.UID == "" || string(g.UID) != a.GroupUID || g.Spec.Lifecycle.IsStopped() || g.Annotations[names.AnnotationLifecycleRetirement] != "" {
			return fmt.Errorf("group is stopped or identity changed")
		}
		if err := rejectTerminating(kindLabGroup, g); err != nil {
			return err
		}
		a.Token = admissionToken(a)
		if old := g.Spec.Admission; old != nil {
			if equality.Semantic.DeepEqual(old, a) {
				return nil
			}
			return pendingAdmissionError{group: group, token: old.Token}
		}
		g.Spec.Admission = a.DeepCopy()
		_, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, g, metav1.UpdateOptions{})
		return err
	})
}
func (h *Handler) finishChildAdmission(ctx context.Context, group string, a *lab.GroupChildAdmission) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		g, err := h.getGroup(ctx, group)
		if err != nil {
			return err
		}
		if string(g.UID) != a.GroupUID {
			return fmt.Errorf("group admission identity changed")
		}
		if g.Spec.Admission == nil {
			return nil
		}
		if !equality.Semantic.DeepEqual(g.Spec.Admission, a) {
			return fmt.Errorf("child admission was superseded")
		}
		if !h.childAdmissionWritten(ctx, g, a) {
			return fmt.Errorf("child admission write not confirmed")
		}
		g.Spec.Admission = nil
		_, err = h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, g, metav1.UpdateOptions{})
		return err
	})
}

// childAdmissionWritten may resolve a crash after the durable child write. A
// never-written operation stays pending until an identical RPC retry completes.
func (h *Handler) childAdmissionWritten(ctx context.Context, g *lab.LabGroup, a *lab.GroupChildAdmission) bool {
	if a == nil || a.GroupUID != string(g.UID) || g.Status.Namespace == "" {
		return false
	}
	l, err := h.cs.LaboratoryV1alpha1().Labs(g.Status.Namespace).Get(ctx, a.LabName, metav1.GetOptions{})
	if err != nil || !ownedBy(tenantOf(ctx), l) || !l.DeletionTimestamp.IsZero() {
		return false
	}
	if a.SpecHash != "" {
		if birthWriteReady(l) != nil {
			return false
		}
		return l.Annotations[names.AnnotationSpecHash] == a.SpecHash
	}
	i := l.Spec.Lifecycle
	return string(l.UID) == a.ExpectedLabUID && i != nil && (i.OperationID == a.OperationID && i.Revision == a.Revision && i.DesiredState == a.DesiredState || i.IsStopped() && i.Revision >= a.Revision)
}

type pendingAdmissionError struct{ group, token string }

func (e pendingAdmissionError) Error() string {
	return fmt.Sprintf("group %q has pending child admission %s; retry the same admitted create/start", e.group, e.token)
}
