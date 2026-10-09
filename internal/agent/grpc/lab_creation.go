package grpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	labclient "github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func (h *Handler) prepareLabBirth(ctx context.Context, g *lab.LabGroup, it *protobuf.LabItem, v *labVariant) (*lab.LabCreationReceipt, error) {
	if it.GetExpectedGroupUid() == "" {
		return nil, nil
	}
	life := v.spec.Lifecycle
	if life == nil || life.DesiredState != lifecycleRunning || life.OperationID == "" || life.Revision != 1 {
		return nil, fmt.Errorf("fenced birth requires explicit initial Running revision1")
	}
	ns, err := h.k8s.CoreV1().Namespaces().Get(ctx, g.Status.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if ns.UID == "" {
		return nil, fmt.Errorf("birth namespace UID unavailable")
	}
	hash, err := labclient.CreationDefinitionHash(v.rawDefinition, it.GetDeployGroup(), it.GetDeployAfter())
	if err != nil {
		return nil, err
	}
	var receipt *lab.LabCreationReceipt
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.getGroup(ctx, it.GetLabGroup())
		if err != nil {
			return err
		}
		if current.UID != g.UID || current.Status.Namespace != ns.Name || current.Spec.Admission == nil {
			return fmt.Errorf("birth group admission changed")
		}
		for i := range current.Status.Creations {
			old := current.Status.Creations[i]
			if old.LabName == crName(it.GetName()) && old.OperationID == life.OperationID && old.Revision == life.Revision {
				if old.GroupUID != string(g.UID) || old.NamespaceUID != string(ns.UID) || old.DefinitionHash != hash {
					return fmt.Errorf("original birth identity changed")
				}
				// A pending replay may observe only the originally stamped object. It
				// cannot issue another Create after an unknown original outcome.
				actual, err := h.cs.LaboratoryV1alpha1().Labs(ns.Name).Get(ctx, old.LabName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("original birth is pending or absent; cannot recreate")
				}
				var stamped lab.LabCreationReceipt
				if json.Unmarshal([]byte(actual.Annotations[names.AnnotationLabCreation]), &stamped) != nil || stamped.CreationID != old.CreationID || old.Committed && old.LabUID != string(actual.UID) {
					return fmt.Errorf("original birth object was replaced")
				}
				receipt = &old
				return nil
			}
		}
		if len(current.Status.Creations) >= 5000 {
			return fmt.Errorf("group birth receipt ledger full")
		}
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		receipt = &lab.LabCreationReceipt{LabName: crName(it.GetName()), GroupUID: string(g.UID), NamespaceUID: string(ns.UID), OperationID: life.OperationID, Revision: life.Revision, DefinitionHash: hash, CreationID: hex.EncodeToString(random)}
		current.Status.Creations = append(current.Status.Creations, *receipt)
		_, err = h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return err
	})
	return receipt, err
}
func (h *Handler) commitLabBirth(ctx context.Context, group string, birth *lab.LabCreationReceipt, created *lab.Lab, first bool) error {
	if created.UID == "" {
		return fmt.Errorf("actual created UID missing")
	}
	var stamped lab.LabCreationReceipt
	if json.Unmarshal([]byte(created.Annotations[names.AnnotationLabCreation]), &stamped) != nil || stamped.CreationID != birth.CreationID {
		return fmt.Errorf("cannot adopt an unknown existing birth")
	}
	if birth.Committed && birth.LabUID != string(created.UID) {
		return fmt.Errorf("original birth UID changed")
	}
	_ = first
	birth.LabUID = string(created.UID)
	birth.Committed = true
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		g, err := h.getGroup(ctx, group)
		if err != nil {
			return err
		}
		if string(g.UID) != birth.GroupUID {
			return fmt.Errorf("birth group UID changed")
		}
		for i := range g.Status.Creations {
			old := &g.Status.Creations[i]
			if old.CreationID == birth.CreationID {
				if old.Committed && old.LabUID != birth.LabUID {
					return fmt.Errorf("birth UID already bound")
				}
				*old = *birth
				_, err = h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(ctx, g, metav1.UpdateOptions{})
				return err
			}
		}
		return fmt.Errorf("birth receipt disappeared")
	}); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.cs.LaboratoryV1alpha1().Labs(created.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != created.UID {
			return fmt.Errorf("birth object replaced before receipt commit")
		}
		raw, _ := json.Marshal(birth)
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[names.AnnotationLabCreation] = string(raw)
		_, err = h.cs.LaboratoryV1alpha1().Labs(created.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}
func creationReceiptToProto(l *lab.Lab) *protobuf.LabCreationReceipt {
	var birth lab.LabCreationReceipt
	if json.Unmarshal([]byte(l.Annotations[names.AnnotationLabCreation]), &birth) != nil || !birth.Committed || birth.LabUID != string(l.UID) || birth.CreationID == "" {
		return nil
	}
	return &protobuf.LabCreationReceipt{GroupUid: birth.GroupUID, NamespaceUid: birth.NamespaceUID, OperationId: birth.OperationID, Revision: birth.Revision, DefinitionHash: birth.DefinitionHash, CreationId: birth.CreationID, LabUid: birth.LabUID, Committed: true}
}

func birthWriteReady(l *lab.Lab) error {
	raw := l.Annotations[names.AnnotationLabCreation]
	if raw == "" {
		return nil
	}
	var receipt lab.LabCreationReceipt
	if json.Unmarshal([]byte(raw), &receipt) != nil || !receipt.Committed || receipt.LabUID != string(l.UID) {
		return fmt.Errorf("original birth admission is pending")
	}
	return nil
}
