package laboratory

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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

// tenantPullSecret is the name of the registry Secret a tenant brings, in the tenants
// namespace; "" when the tenant has none or does not exist.
func tenantPullSecret(ctx context.Context, c client.Reader, tenantName string) (string, error) {
	var t laboratoryv1alpha1.Tenant
	if err := c.Get(ctx, types.NamespacedName{Name: tenantName}, &t); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return t.Spec.Images.PullSecret, nil
}

// devicePullSecrets are the pull secrets of a device pod: the tenant's own credentials when
// it has any (copied into the group namespace by the LabGroup controller), and never the
// platform's. A tenant that cannot be read counts as having none: the pod then pulls
// anonymously and fails to pull a private image, which is the safe direction.
func (r *DeviceReconciler) devicePullSecrets(ctx context.Context, device *laboratoryv1alpha1.Device) []corev1.LocalObjectReference {
	secret, err := tenantPullSecret(ctx, r.Client, names.TenantOf(device.Labels))
	if err != nil || secret == "" {
		return nil
	}
	return pullSecretRefs([]string{names.TenantPullSecret})
}

// syncTenantPullSecret makes the registry credentials of the group's tenant available in the
// group namespace as names.TenantPullSecret, and removes the copy when the tenant has none
// (any more). The source is a Secret of the tenants namespace; a missing or non-registry
// source is an error, so the group retries instead of starting pods that cannot pull.
func syncTenantPullSecret(ctx context.Context, c client.Client, tenantName, ns string) error {
	secret, err := tenantPullSecret(ctx, c, tenantName)
	if err != nil {
		return err
	}
	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.TenantPullSecret, Namespace: ns}}
	if secret == "" {
		return client.IgnoreNotFound(c.Delete(ctx, dst))
	}
	var src corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: secret, Namespace: names.TenantsNamespace}, &src); err != nil {
		return fmt.Errorf("tenant %s image pull secret %s/%s: %w", tenantName, names.TenantsNamespace, secret, err)
	}
	if src.Type != corev1.SecretTypeDockerConfigJson {
		return fmt.Errorf("tenant %s image pull secret %s/%s has type %q, want %q",
			tenantName, names.TenantsNamespace, secret, src.Type, corev1.SecretTypeDockerConfigJson)
	}
	_, err = controllerutil.CreateOrUpdate(ctx, c, dst, func() error {
		if dst.Labels == nil {
			dst.Labels = map[string]string{}
		}
		dst.Labels["app.kubernetes.io/managed-by"] = "laboratory-operator"
		dst.Type = src.Type
		dst.Data = src.Data
		return nil
	})
	if err != nil {
		return fmt.Errorf("copy tenant %s image pull secret to %s: %w", tenantName, ns, err)
	}
	return nil
}
