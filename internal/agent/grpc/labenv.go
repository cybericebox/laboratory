package grpc

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// reconcileEnvSecrets materializes per-device environment variables into
// write-only Secrets named "<lab>-<device>-env", owner-referenced by the Lab so
// they are garbage-collected with it. The device pod loads them via envFrom.
//
// The agent only ever CREATEs/DELETEs these Secrets — never reads them — so a
// device's env values (e.g. a task flag or a license) live solely in the
// Secret, never in the Lab/Device CR and never back through the agent.
// "Overwrite" is therefore delete-then-create, which needs no read.
//
// For every device in the lab spec: write its Secret when env is provided,
// otherwise ensure no Secret exists (a device that dropped its env).
func (h *Handler) reconcileEnvSecrets(ctx context.Context, lab *laboratoryv1alpha1.Lab, env []*protobuf.DeviceEnv) error {
	byDevice := make(map[string]map[string]string, len(env))
	for _, de := range env {
		if de != nil {
			byDevice[de.Device] = de.Vars
		}
	}

	controller := true
	owner := metav1.OwnerReference{
		APIVersion:         laboratoryv1alpha1.SchemeGroupVersion.String(),
		Kind:               "Lab",
		Name:               lab.Name,
		UID:                lab.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &controller,
	}

	secrets := h.k8s.CoreV1().Secrets(lab.Namespace)
	for i := range lab.Spec.Devices {
		dev := lab.Spec.Devices[i].Name
		name := lab.Name + "-" + dev + "-env"

		// Delete first so overwrite never requires reading the Secret.
		if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		vars := byDevice[dev]
		if len(vars) == 0 {
			continue
		}
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       lab.Namespace,
				OwnerReferences: []metav1.OwnerReference{owner},
				Labels: map[string]string{
					names.LabelLab:    lab.Name,
					names.LabelDevice: dev,
				},
			},
			StringData: vars,
		}
		if _, err := secrets.Create(ctx, s, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	return nil
}
