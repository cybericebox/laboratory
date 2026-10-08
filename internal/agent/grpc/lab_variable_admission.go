package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func variableWriteToken(parent *lab.Lab, devices []string, env deviceVars) string {
	raw, _ := json.Marshal(struct {
		UID     string
		Devices []string
		Env     deviceVars
	}{string(parent.UID), devices, env})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (h *Handler) claimVariableWrite(ctx context.Context, parent *lab.Lab, token string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.cs.LaboratoryV1alpha1().Labs(parent.Namespace).Get(ctx, parent.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := birthWriteReady(current); err != nil {
			return err
		}
		if current.UID != parent.UID || !current.DeletionTimestamp.IsZero() || current.Annotations[names.AnnotationLifecycleRetirement] != "" {
			return fmt.Errorf("Lab is retired, terminating or identity changed")
		}
		if old := current.Annotations[names.AnnotationLabVariableAdmission]; old != "" {
			if old == token {
				return nil
			}
			return fmt.Errorf("another Lab variable write is pending; replay the original write")
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[names.AnnotationLabVariableAdmission] = token
		_, err = h.cs.LaboratoryV1alpha1().Labs(parent.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}
func (h *Handler) finishVariableWrite(ctx context.Context, parent *lab.Lab, token string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.cs.LaboratoryV1alpha1().Labs(parent.Namespace).Get(ctx, parent.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != parent.UID || current.Annotations[names.AnnotationLifecycleRetirement] != "" || current.Annotations[names.AnnotationLabVariableAdmission] != token {
			return fmt.Errorf("Lab variable admission identity changed")
		}
		delete(current.Annotations, names.AnnotationLabVariableAdmission)
		_, err = h.cs.LaboratoryV1alpha1().Labs(parent.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}
