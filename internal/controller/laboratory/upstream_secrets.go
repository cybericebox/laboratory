package laboratory

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// The credentials of the upstream registries live in one namespace, laboratory-upstream, which the registry gate reads and nothing else:
// no pod references them and no node can read them (the spec "pools-release", 2.4.5). The operator writes two kinds of Secret there, the
// tenant's (tenant-<name>, from Tenant.spec.images.pullSecret) and the platform's own (platform, from the operator's imagePullSecrets); the
// agent writes the third kind, the lab's own (lab-<hash of namespace/lab>). Every Secret is level-triggered: it follows its source on each
// reconcile and goes away with it.

func upstreamSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: names.UpstreamNamespace}}
}

// writeUpstream creates or updates a dockerconfigjson Secret of the upstream namespace.
func writeUpstream(ctx context.Context, c client.Client, name string, doc []byte) error {
	dst := upstreamSecret(name)
	_, err := controllerutil.CreateOrUpdate(ctx, c, dst, func() error {
		dst.Labels = map[string]string{managedByLabel: managedByValue}
		dst.Type = corev1.SecretTypeDockerConfigJson
		dst.Data = map[string][]byte{corev1.DockerConfigJsonKey: doc}
		return nil
	})
	if err != nil {
		return fmt.Errorf("write %s/%s: %w", names.UpstreamNamespace, name, err)
	}
	return nil
}

func deleteUpstream(ctx context.Context, c client.Client, name string) error {
	return client.IgnoreNotFound(c.Delete(ctx, upstreamSecret(name)))
}

// syncUpstreamTenant makes tenant-<name> of the upstream namespace from the Secret the tenant's spec names (in the tenants namespace),
// and removes it when the tenant has none (any more) or the image cache is off. A source that is missing or is not a registry Secret
// leaves the copy as it is and is returned as an error, so the tenant is looked at again.
func (r *TenantReconciler) syncUpstreamTenant(ctx context.Context, t *laboratoryv1alpha1.Tenant) error {
	name := names.UpstreamTenantSecret(t.Name)
	if !r.CacheEnabled || t.Spec.Images.PullSecret == "" {
		return deleteUpstream(ctx, r.Client, name)
	}
	var src corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: t.Spec.Images.PullSecret, Namespace: names.TenantsNamespace}, &src); err != nil {
		return fmt.Errorf("tenant %s image pull secret %s/%s: %w", t.Name, names.TenantsNamespace, t.Spec.Images.PullSecret, err)
	}
	if src.Type != corev1.SecretTypeDockerConfigJson {
		return fmt.Errorf("tenant %s image pull secret %s/%s has type %q, want %q", t.Name, names.TenantsNamespace, src.Name, src.Type, corev1.SecretTypeDockerConfigJson)
	}
	return writeUpstream(ctx, r.Client, name, src.Data[corev1.DockerConfigJsonKey])
}

// syncUpstreamPlatform makes the Secret "platform" of the upstream namespace from the operator's imagePullSecrets (in the operator's
// namespace): one dockerconfigjson with the logins of all of them, a later one over an earlier one for the same registry. With the cache
// off, or no secret configured, the Secret is removed. A missing or non-registry source is an error, never skipped.
func syncUpstreamPlatform(ctx context.Context, c client.Client, cacheOn bool, secretNames []string) error {
	if !cacheOn || len(secretNames) == 0 {
		return deleteUpstream(ctx, c, names.UpstreamPlatformSecret)
	}
	auths := map[string]json.RawMessage{}
	for _, name := range secretNames {
		var src corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: names.SystemNamespace}, &src); err != nil {
			return fmt.Errorf("image pull secret %s/%s: %w", names.SystemNamespace, name, err)
		}
		var raw []byte
		switch src.Type {
		case corev1.SecretTypeDockerConfigJson:
			raw = src.Data[corev1.DockerConfigJsonKey]
		case corev1.SecretTypeDockercfg: // the old form is the auths map itself
			raw = append(append([]byte(`{"auths":`), src.Data[corev1.DockerConfigKey]...), '}')
		default:
			return fmt.Errorf("image pull secret %s/%s has type %q, want %q or %q",
				names.SystemNamespace, name, src.Type, corev1.SecretTypeDockerConfigJson, corev1.SecretTypeDockercfg)
		}
		var doc struct {
			Auths map[string]json.RawMessage `json:"auths"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("image pull secret %s/%s: %w", names.SystemNamespace, name, err)
		}
		for host, a := range doc.Auths {
			auths[host] = a
		}
	}
	// json.Marshal sorts map keys: the same input always gives the same document.
	doc, err := json.Marshal(struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}{auths})
	if err != nil {
		return err
	}
	return writeUpstream(ctx, c, names.UpstreamPlatformSecret, doc)
}

// deleteUpstreamLab removes the credentials Secret of a lab that is gone (the agent wrote it; a Secret of another namespace cannot be
// owned by the Lab, so the operator deletes it).
func deleteUpstreamLab(ctx context.Context, c client.Client, namespace, lab string) error {
	return deleteUpstream(ctx, c, names.UpstreamLabSecret(namespace, lab))
}

// removeLegacyPullSecrets deletes, from a group namespace, the pull Secrets earlier versions made there: the tenant's copy, the platform's
// re-keyed one, the re-keyed ones of labs and the copies of the operator's own. No pod references any of them any more.
func removeLegacyPullSecrets(ctx context.Context, c client.Client, ns string) error {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		return err
	}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Type != corev1.SecretTypeDockerConfigJson && s.Type != corev1.SecretTypeDockercfg {
			continue
		}
		if err := client.IgnoreNotFound(c.Delete(ctx, s)); err != nil {
			return fmt.Errorf("remove the image pull secret %s/%s of an earlier version: %w", ns, s.Name, err)
		}
	}
	return nil
}

// removeLegacyPullSecretsWhenConverged is removeLegacyPullSecrets once the VPN and gateway Deployments of the group reference no image pull
// secret (a Deployment of an earlier version still does, until the rollout has converged it): deleting a Secret a pod references would stop
// the pod from being pulled again.
func (r *LabGroupReconciler) removeLegacyPullSecretsWhenConverged(ctx context.Context, ns string) error {
	for _, name := range []string{names.ComponentVPN, names.ComponentGateway} {
		var d appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if len(d.Spec.Template.Spec.ImagePullSecrets) > 0 {
			return nil // not converged yet: a later reconcile does it
		}
	}
	return removeLegacyPullSecrets(ctx, r.Client, ns)
}
