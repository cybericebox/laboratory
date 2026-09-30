package laboratory

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/cybericebox/laboratory/internal/names"
)

// pullSecretRefs turns the configured pull secret names into the references a
// pod spec takes; nil when none are configured.
func pullSecretRefs(secretNames []string) []corev1.LocalObjectReference {
	var out []corev1.LocalObjectReference
	for _, n := range secretNames {
		out = append(out, corev1.LocalObjectReference{Name: n})
	}
	return out
}

// copyPullSecrets makes every configured registry Secret of the operator's
// namespace available in a group namespace, where the VPN, gateway and device
// pods run. The Secrets themselves are created outside the chart; a missing or
// non-registry source is an error, never skipped, because pods would then fail to
// pull with a confusing message. Copies follow the source on each reconcile.
func copyPullSecrets(ctx context.Context, c client.Client, secretNames []string, ns string) error {
	for _, name := range secretNames {
		var src corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: names.SystemNamespace}, &src); err != nil {
			return fmt.Errorf("image pull secret %s/%s: %w", names.SystemNamespace, name, err)
		}
		if src.Type != corev1.SecretTypeDockerConfigJson && src.Type != corev1.SecretTypeDockercfg {
			return fmt.Errorf("image pull secret %s/%s has type %q, want %q or %q",
				names.SystemNamespace, name, src.Type, corev1.SecretTypeDockerConfigJson, corev1.SecretTypeDockercfg)
		}
		dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if _, err := controllerutil.CreateOrUpdate(ctx, c, dst, func() error {
			if dst.Labels == nil {
				dst.Labels = map[string]string{}
			}
			dst.Labels["app.kubernetes.io/managed-by"] = "laboratory-operator"
			dst.Type = src.Type
			dst.Data = src.Data
			return nil
		}); err != nil {
			return fmt.Errorf("copy image pull secret %s to %s: %w", name, ns, err)
		}
	}
	return nil
}
