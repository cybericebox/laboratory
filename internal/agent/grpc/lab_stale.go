package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
)

// A Lab name is deterministic, so a recreate of a lab (a new row of the platform for the same slot) meets the Lab of the
// previous generation. Creation operation ids are UUIDv7 (time ordered): the Lab of an older operation, of the same tenant, in
// the same group namespace, is stale and the agent replaces it (deletes it; the caller retries the create, as for a terminating
// object). A newer Lab, or one with no comparable generation, is never touched: the caller gets LAB_NAME_CONFLICT.

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// labCreationOperation is the creation operation of a Lab: the stamped birth receipt, else the lifecycle operation.
func labCreationOperation(l *laboratoryv1alpha1.Lab) string {
	var receipt laboratoryv1alpha1.LabCreationReceipt
	if json.Unmarshal([]byte(l.Annotations[names.AnnotationLabCreation]), &receipt) == nil && receipt.OperationID != "" {
		return receipt.OperationID
	}
	if l.Spec.Lifecycle != nil {
		return l.Spec.Lifecycle.OperationID
	}
	return ""
}

// replaceStaleLab handles a Lab that holds the name of a lab being created under operation op. nil: no stale Lab in the way.
func (h *Handler) replaceStaleLab(ctx context.Context, ns, id, op string) error {
	if op == "" {
		return nil
	}
	cur, err := h.cs.LaboratoryV1alpha1().Labs(ns).Get(ctx, crName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.DeletionTimestamp != nil {
		return terminatingError(kindLab, id)
	}
	old := labCreationOperation(cur)
	if old == op || names.IDOf(cur) != id {
		return nil
	}
	if !ownedBy(tenantOf(ctx), cur) {
		return nil
	}
	if !uuidV7.MatchString(old) || !uuidV7.MatchString(op) || old > op {
		return reasonError{reason: client.ReasonLabNameConflict, text: fmt.Sprintf("%s %s exists from operation %q, which cannot be replaced by operation %q", kindLab, id, old, op)}
	}
	uid, rv := cur.UID, cur.ResourceVersion
	if err := h.cs.LaboratoryV1alpha1().Labs(ns).Delete(ctx, cur.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	log.Printf("lab %s/%s: replacing the stale Lab of operation %s with operation %s", ns, id, old, op)
	return reasonError{reason: client.ReasonStaleLabReplacing, text: fmt.Sprintf("%s %s of an older generation is being replaced, retry later", kindLab, id), retryable: true}
}
