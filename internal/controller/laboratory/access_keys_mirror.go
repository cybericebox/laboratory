package laboratory

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// AccessKeysMirror keeps the ConfigMap names.AccessKeysConfigMap of the device access service the same as the access public keys of the
// tenants. The device access service verifies a console grant with the tenant's key; it has no API token and must not get one, so the
// operator copies the keys (public data) into a ConfigMap the service mounts as files. The copy is level-triggered from the Secrets of
// names.AccessKeysNamespace, whatever changed them (a key added or rotated, RemoveAccessKey, a tenant deleted): one fixed key, one pass
// lists the Secrets and writes the ConfigMap only when it differs. A key removed from a Secret leaves the ConfigMap in the same pass; the
// service then stops accepting it after the kubelet's volume refresh (about a minute) and its own reread interval.
type AccessKeysMirror struct {
	Client client.Client
	// Reader reads without a cache: the operator reads no Secret or ConfigMap through an informer of the whole cluster.
	Reader client.Reader
	// Namespace is the namespace of the device access service, where the ConfigMap is.
	Namespace string
}

// Reconcile copies the keys.
func (m *AccessKeysMirror) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	var secrets corev1.SecretList
	if err := m.Reader.List(ctx, &secrets, client.InNamespace(names.AccessKeysNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list the access key Secrets: %w", err)
	}
	want := accessKeyEntries(secrets.Items)

	var cm corev1.ConfigMap
	err := m.Reader.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: names.AccessKeysConfigMap}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: names.AccessKeysConfigMap}, Data: want}
		if err := m.Client.Create(ctx, &cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create the access keys ConfigMap: %w", err)
		}
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("read the access keys ConfigMap: %w", err)
	}
	if maps.Equal(cm.Data, want) && len(cm.BinaryData) == 0 {
		return ctrl.Result{}, nil
	}
	cm.Data, cm.BinaryData = want, nil
	if err := m.Client.Update(ctx, &cm); err != nil {
		return ctrl.Result{}, fmt.Errorf("update the access keys ConfigMap: %w", err)
	}
	return ctrl.Result{}, nil
}

// accessKeyEntries is the data of the ConfigMap for these Secrets: an entry <tenant>.<key id> for every key that parses as an Ed25519
// public key. Whatever else a Secret of that namespace holds (a name that is no tenant's, a key id that cannot be a file name, a value that
// is no public key) is left out: the mirror only ever carries what the proxy would accept as a key.
func accessKeyEntries(secrets []corev1.Secret) map[string]string {
	out := map[string]string{}
	for i := range secrets {
		s := &secrets[i]
		tenant, ok := names.AccessKeysSecretTenant(s.Name)
		if !ok || !grant.ValidTenant(tenant) {
			continue
		}
		for keyID, pem := range s.Data {
			if _, ok := grant.ParseAccessKey(pem); !ok || !grant.ValidKeyID(keyID) {
				continue
			}
			out[names.AccessKeysConfigMapKey(tenant, keyID)] = string(pem)
		}
	}
	return out
}

// SetupWithManager reconciles one fixed key on every change of a Secret of the access keys namespace.
func (m *AccessKeysMirror) SetupWithManager(mgr ctrl.Manager) error {
	key := reconcile.Request{NamespacedName: types.NamespacedName{Name: "access-keys"}}
	toKey := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request { return []reconcile.Request{key} })
	inNamespace := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == names.AccessKeysNamespace })
	return ctrl.NewControllerManagedBy(mgr).Named("access-keys-mirror").
		Watches(&corev1.Secret{}, toKey, builder.WithPredicates(inNamespace)).
		Complete(reconcileutil.Quiet(m))
}
