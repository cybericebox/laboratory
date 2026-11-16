package laboratory

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// pullSecretRefs turns the configured pull secret names into the references a
// pod spec takes; nil when none are configured.
func pullSecretRefs(secretNames []string) []corev1.LocalObjectReference {
	if len(secretNames) == 0 {
		return nil
	}
	out := make([]corev1.LocalObjectReference, 0, len(secretNames))
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

// syncPlatformCacheSecret makes the pull Secret of the platform's own pods (VPN, gateway) of a group namespace for the image cache:
// the operator's pull secrets re-keyed for the cache path, tenant name imagecache.PlatformTenant. With the cache off, or nothing to
// re-key, the Secret is removed.
func syncPlatformCacheSecret(ctx context.Context, c client.Client, mirror imagecache.Rewriter, secretNames []string, ns string) error {
	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.PlatformCacheSecret, Namespace: ns}}
	if !mirror.Enabled() || len(secretNames) == 0 {
		return client.IgnoreNotFound(c.Delete(ctx, dst))
	}
	var docs [][]byte
	for _, name := range secretNames {
		var src corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: names.SystemNamespace}, &src); err != nil {
			return fmt.Errorf("image pull secret %s/%s: %w", names.SystemNamespace, name, err)
		}
		switch src.Type {
		case corev1.SecretTypeDockerConfigJson:
			docs = append(docs, src.Data[corev1.DockerConfigJsonKey])
		case corev1.SecretTypeDockercfg: // the old form is the auths map itself
			docs = append(docs, append(append([]byte(`{"auths":`), src.Data[corev1.DockerConfigKey]...), '}'))
		}
	}
	doc, n, err := imagecache.RekeyForCache(mirror.Prefix, imagecache.PlatformTenant, mirror.Registries, docs...)
	if err != nil {
		return fmt.Errorf("pull secret of the image cache for the platform pods: %w", err)
	}
	if n == 0 {
		return client.IgnoreNotFound(c.Delete(ctx, dst))
	}
	_, err = controllerutil.CreateOrUpdate(ctx, c, dst, func() error {
		dst.Labels = map[string]string{"app.kubernetes.io/managed-by": "laboratory-operator"}
		dst.Type = corev1.SecretTypeDockerConfigJson
		dst.Data = map[string][]byte{corev1.DockerConfigJsonKey: doc}
		return nil
	})
	if err != nil {
		return fmt.Errorf("write the platform pull secret of the image cache to %s: %w", ns, err)
	}
	return nil
}

// groupPullSecretRefs are the pull secrets of the VPN and gateway pods of a group: the operator's own secrets as they are, or, when the
// pods pull through the image cache, those secrets re-keyed for it (syncPlatformCacheSecret).
func (r *LabGroupReconciler) groupPullSecretRefs() []corev1.LocalObjectReference {
	if r.Mirror.Enabled() && len(r.ImagePullSecrets) > 0 {
		return []corev1.LocalObjectReference{{Name: names.PlatformCacheSecret}}
	}
	return pullSecretRefs(r.ImagePullSecrets)
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

// errPullSecretPending: the lab names a pull Secret that does not exist yet. The agent
// creates it right after the Lab (its owner reference needs the Lab's UID), so this is a
// short wait, retried, never a failure of the device.
var errPullSecretPending = errors.New("the lab's image pull secret does not exist yet")

// pullSecretPendingError is errPullSecretPending with the Secret it waits for.
type pullSecretPendingError struct{ namespace, lab, secret string }

func (e *pullSecretPendingError) Error() string {
	return fmt.Sprintf("lab %s/%s image pull secret %s: %s", e.namespace, e.lab, e.secret, errPullSecretPending)
}

func (e *pullSecretPendingError) Is(target error) bool { return target == errPullSecretPending }

// pullSecretRetry is how soon a device that waits for its lab's pull Secret is reconciled
// again: the Device controller does not watch Secrets.
const pullSecretRetry = 2 * time.Second

// devicePullSecrets are the pull secrets of a device pod: the tenant's own credentials when
// it has any (copied into the group namespace by the LabGroup controller) and the lab's own
// (written by the agent into the lab's namespace), never the platform's. A tenant that
// cannot be read counts as having none: the pod then pulls anonymously and fails to pull a
// private image, which is the safe direction. The lab's Secret must exist (errPullSecretPending).
func (r *DeviceReconciler) devicePullSecrets(ctx context.Context, device *laboratoryv1alpha1.Device) ([]corev1.LocalObjectReference, error) {
	var secrets []string
	tenantOwn := false
	if secret, err := tenantPullSecret(ctx, r.Client, names.TenantOf(device.Labels)); err == nil && secret != "" {
		secrets = append(secrets, names.TenantPullSecret)
		tenantOwn = true
	}
	own, err := r.labPullSecret(ctx, device)
	if err != nil {
		return nil, err
	}
	if own != "" {
		secrets = append(secrets, own)
	}
	if device.Spec.ImageMirror != "" {
		// A device on the image cache: its credentials are the raw ones re-keyed for the cache path, never the raw ones themselves.
		return r.cachePullSecrets(ctx, device, tenantOwn, own)
	}
	return pullSecretRefs(secrets), nil
}

// cachePullSecrets makes the pull Secret of a device on the image cache from the tenant's Secret (when the tenant has one) and the lab's
// own (the agent wrote them in the lab's namespace): the same credentials under the keys "<prefix>/c/<registry>" with the username
// "<tenant>~<user>", the lab's own over the tenant's for one registry. The tenant is the tenant label of the device, set by the agent from
// the caller's certificate; the caller sends no name. No credentials, or only for registries the cache does not serve: no Secret and no
// reference (the pod pulls anonymously through the cache). The Secret belongs to the lab and goes with it.
func (r *DeviceReconciler) cachePullSecrets(ctx context.Context, device *laboratoryv1alpha1.Device, tenantOwn bool, own string) ([]corev1.LocalObjectReference, error) {
	var docs [][]byte
	for _, raw := range []struct {
		name string
		use  bool
	}{{names.TenantPullSecret, tenantOwn}, {own, own != ""}} {
		if !raw.use {
			continue
		}
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: device.Namespace, Name: raw.name}, &s); err != nil {
			return nil, err
		}
		docs = append(docs, s.Data[corev1.DockerConfigJsonKey])
	}
	lab := device.Spec.LabRef
	if lab == "" {
		lab = device.Name
	}
	name := names.LabCacheRegistrySecret(lab)
	doc, n, err := imagecache.RekeyForCache(device.Spec.ImageMirror, names.TenantOf(device.Labels), r.MirrorRegistries, docs...)
	if err != nil {
		return nil, fmt.Errorf("pull secret of the image cache: %w", err)
	}
	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: device.Namespace}}
	if n == 0 {
		return nil, client.IgnoreNotFound(r.Delete(ctx, dst))
	}
	owner, err := r.cacheSecretOwner(ctx, device)
	if err != nil {
		return nil, err
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dst, func() error {
		dst.Labels = map[string]string{"app.kubernetes.io/managed-by": "laboratory-operator", names.LabelLab: lab}
		dst.OwnerReferences = []metav1.OwnerReference{owner}
		dst.Type = corev1.SecretTypeDockerConfigJson
		dst.Data = map[string][]byte{corev1.DockerConfigJsonKey: doc}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("pull secret %s of the image cache: %w", name, err)
	}
	return []corev1.LocalObjectReference{{Name: name}}, nil
}

// cacheSecretOwner is the controller reference of the cache pull Secret: the lab, or the device itself when it has none.
func (r *DeviceReconciler) cacheSecretOwner(ctx context.Context, device *laboratoryv1alpha1.Device) (metav1.OwnerReference, error) {
	controller := true
	if device.Spec.LabRef != "" {
		var lab laboratoryv1alpha1.Lab
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: device.Namespace, Name: device.Spec.LabRef}, &lab); err == nil {
			return metav1.OwnerReference{APIVersion: laboratoryv1alpha1.SchemeGroupVersion.String(), Kind: "Lab", Name: lab.Name, UID: lab.UID, Controller: &controller}, nil
		} else if !apierrors.IsNotFound(err) {
			return metav1.OwnerReference{}, err
		}
	}
	return metav1.OwnerReference{APIVersion: laboratoryv1alpha1.SchemeGroupVersion.String(), Kind: "Device", Name: device.Name, UID: device.UID, Controller: &controller}, nil
}

// labPullSecret is the name of the registry Secret of the device's lab (Spec.ImagePullSecret,
// set only by the agent); "" when the lab has none or is gone.
func (r *DeviceReconciler) labPullSecret(ctx context.Context, device *laboratoryv1alpha1.Device) (string, error) {
	key := client.ObjectKeyFromObject(device)
	if device.Spec.LabRef == "" {
		r.pullWaiting.Delete(key)
		return "", nil
	}
	var lab laboratoryv1alpha1.Lab
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: device.Namespace, Name: device.Spec.LabRef}, &lab); err != nil {
		if apierrors.IsNotFound(err) {
			r.pullWaiting.Delete(key)
		}
		return "", client.IgnoreNotFound(err)
	}
	name := lab.Spec.ImagePullSecret
	if name == "" {
		r.pullWaiting.Delete(key)
		return "", nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: device.Namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &pullSecretPendingError{namespace: device.Namespace, lab: lab.Name, secret: name}
		}
		return "", err
	}
	r.pullWaiting.Delete(client.ObjectKeyFromObject(device))
	return name, nil
}

// applyPullSecrets gives a device pod its pull secrets (devicePullSecrets).
func (r *DeviceReconciler) applyPullSecrets(ctx context.Context, device *laboratoryv1alpha1.Device, spec *corev1.PodSpec) error {
	refs, err := r.devicePullSecrets(ctx, device)
	if err != nil {
		return err
	}
	setPullSecrets(spec, refs)
	return nil
}

// setPullSecrets puts the pull secrets on a pod spec and, when there are any, makes every container pull with IfNotPresent turned
// into Always: an image pulled with tenant (or lab) A's credentials stays on the node, and with IfNotPresent a pod of B that names the exact
// same reference would run it without any credential. Always makes the node ask the registry, which B cannot satisfy.
func setPullSecrets(spec *corev1.PodSpec, refs []corev1.LocalObjectReference) {
	spec.ImagePullSecrets = refs
	if len(refs) == 0 {
		return
	}
	for i := range spec.InitContainers {
		spec.InitContainers[i].ImagePullPolicy = corev1.PullAlways
	}
	for i := range spec.Containers {
		spec.Containers[i].ImagePullPolicy = corev1.PullAlways
	}
}

// pullSecretWait turns errPullSecretPending into a plain requeue; any other error stays. The
// wait is told once: a Warning event on the Device naming the Secret, and an Info log; the
// repeats (every pullSecretRetry) go to V(1) only. The next wait after the Secret was found
// is told again.
func (r *DeviceReconciler) pullSecretWait(ctx context.Context, device *laboratoryv1alpha1.Device, err error) (ctrl.Result, error) {
	if !errors.Is(err, errPullSecretPending) {
		return ctrl.Result{}, err
	}
	logger := log.FromContext(ctx)
	if _, repeated := r.pullWaiting.LoadOrStore(client.ObjectKeyFromObject(device), struct{}{}); repeated {
		logger.V(1).Info("waiting for the lab's image pull secret", "reason", err.Error())
	} else {
		logger.Info("waiting for the lab's image pull secret", "reason", err.Error())
		if r.Recorder != nil {
			var secret string
			var pending *pullSecretPendingError
			if errors.As(err, &pending) {
				secret = pending.secret
			}
			r.Recorder.Eventf(device, corev1.EventTypeWarning, labstatus.ReasonPullSecretMissing,
				"the image pull secret %s of the lab does not exist yet; waiting for it", secret)
		}
	}
	return ctrl.Result{RequeueAfter: pullSecretRetry}, nil
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
