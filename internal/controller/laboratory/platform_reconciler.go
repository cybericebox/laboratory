package laboratory

import (
	"context"
	"fmt"
	
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/proxy"
)

const (
	// PlatformSecretName is the admin-managed Secret in laboratory-system that
	// holds the platform's lab access public key (Ed25519).
	PlatformSecretName = "lab-access-public-key"
	
	// ProxyCredentialsName is the Secret in laboratory-proxy that the
	// proxy pod mounts. Created and kept in sync by PlatformReconciler.
	ProxyCredentialsName = "lab-access-public-key"
	
	// LabAccessPublicKeyField is the key within both Secrets that holds the
	// Ed25519 public key PEM (a file named public.pem once mounted).
	LabAccessPublicKeyField = "public.pem"
)

// PlatformReconciler watches the admin-managed platform Secret in
// laboratory-system and syncs its lab access public key to laboratory-proxy.
//
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=laboratory-system
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch,namespace=laboratory-proxy
type PlatformReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *PlatformReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("platform")
	
	var src corev1.Secret
	if err := r.Get(
		ctx, types.NamespacedName{
			Name:      PlatformSecretName,
			Namespace: names.SystemNamespace,
		}, &src,
	); err != nil {
		if errors.IsNotFound(err) {
			log.Info("platform secret not found, skipping", "secret", PlatformSecretName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	
	accessKey, ok := src.Data[LabAccessPublicKeyField]
	if !ok {
		return ctrl.Result{}, fmt.Errorf("platform secret missing key %q", LabAccessPublicKeyField)
	}
	
	// Never hand the proxy a key it cannot load (it would keep the old one and
	// log an error, or fail to start).
	if _, err := proxy.ParseLabAccessPublicKey(accessKey); err != nil {
		return ctrl.Result{}, fmt.Errorf("platform secret %q: %w", PlatformSecretName, err)
	}

	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyCredentialsName,
			Namespace: names.ProxyNamespace,
		},
	}
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: ProxyCredentialsName, Namespace: names.ProxyNamespace}, &existing)
	if errors.IsNotFound(err) {
		dst.Data = map[string][]byte{LabAccessPublicKeyField: accessKey}
		if createErr := r.Create(ctx, dst); createErr != nil {
			return ctrl.Result{}, fmt.Errorf("create lab-access-public-key: %w", createErr)
		}
		log.Info("created lab-access-public-key")
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	
	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Data == nil {
		existing.Data = make(map[string][]byte)
	}
	existing.Data[LabAccessPublicKeyField] = accessKey
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch lab-access-public-key: %w", err)
	}
	log.Info("synced lab-access-public-key")
	return ctrl.Result{}, nil
}

func (r *PlatformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch only the specific platform secret — ignore everything else.
	isPlatformSecret := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetName() == PlatformSecretName &&
				e.Object.GetNamespace() == names.SystemNamespace
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetName() == PlatformSecretName &&
				e.ObjectNew.GetNamespace() == names.SystemNamespace
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}, builder.WithPredicates(isPlatformSecret)).
		// Also reconcile when the proxy copy is deleted externally.
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(
				func(_ context.Context, o client.Object) []reconcile.Request {
					if o.GetName() == ProxyCredentialsName && o.GetNamespace() == names.ProxyNamespace {
						return []reconcile.Request{
							{
								NamespacedName: types.NamespacedName{
									Name:      PlatformSecretName,
									Namespace: names.SystemNamespace,
								},
							},
						}
					}
					return nil
				},
			),
		).
		Complete(r)
}
