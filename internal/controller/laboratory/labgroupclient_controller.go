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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

const finalizerLabGroupClient = "cybericebox.com/labgroupclient"

// LabGroupClientReconciler reconciles a LabGroupClient object.
type LabGroupClientReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupclients/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *LabGroupClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lgc laboratoryv1alpha1.LabGroupClient
	if err := r.Get(ctx, req.NamespacedName, &lgc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lgc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lgc)
	}

	return r.reconcileCreate(ctx, &lgc)
}

func (r *LabGroupClientReconciler) reconcileCreate(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(lgc, finalizerLabGroupClient) {
		controllerutil.AddFinalizer(lgc, finalizerLabGroupClient)
		if err := r.Update(ctx, lgc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if lgc.Status.AssignedIP != "" {
		return ctrl.Result{}, nil
	}

	pubKey := lgc.Spec.PublicKey
	var privKeyB64 []byte
	if pubKey == "" {
		priv, pub, err := generateWireGuardKeypair()
		if err != nil {
			return ctrl.Result{}, err
		}
		pubKey = string(pub)
		privKeyB64 = priv
	}

	allocator := poolpkg.NewAllocator(r.Client, "vpn-clients", lgc.Namespace, 254)
	idx, err := allocator.AllocateIndex(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("allocate VPN IP: %w", err)
	}
	assignedIP := fmt.Sprintf("10.8.0.%d/32", idx)

	secretName := fmt.Sprintf("client-%s", lgc.Name)
	if err = r.ensureClientSecret(ctx, lgc, secretName, pubKey, string(privKeyB64), assignedIP); err != nil {
		return ctrl.Result{}, err
	}

	lgc.Status.AssignedIP = assignedIP
	lgc.Status.SecretRef = secretName
	return ctrl.Result{}, r.Status().Update(ctx, lgc)
}

func (r *LabGroupClientReconciler) reconcileDelete(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient) (ctrl.Result, error) {
	if lgc.Status.AssignedIP != "" {
		allocator := poolpkg.NewAllocator(r.Client, "vpn-clients", lgc.Namespace, 254)
		idx, err := ipToIndex(lgc.Status.AssignedIP)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := allocator.ReleaseIndex(ctx, idx); err != nil {
			return ctrl.Result{}, err
		}
	}

	secretName := fmt.Sprintf("client-%s", lgc.Name)
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: lgc.Namespace}, &s); err == nil {
		if err = r.Delete(ctx, &s); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(lgc, finalizerLabGroupClient)
	return ctrl.Result{}, r.Update(ctx, lgc)
}

func (r *LabGroupClientReconciler) ensureClientSecret(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient, name, pubKey, privKey, assignedIP string) error {
	data := map[string][]byte{
		"publicKey":  []byte(pubKey),
		"assignedIP": []byte(assignedIP),
	}
	if privKey != "" {
		data["privateKey"] = []byte(privKey)
	}

	var existing corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lgc.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lgc.Namespace},
		Data:       data,
	}
	return r.Create(ctx, s)
}

// ipToIndex extracts the last octet of a CIDR like "10.8.0.5/32" as pool index.
func ipToIndex(cidr string) (uint, error) {
	var a, b, c, d uint
	if n, err := fmt.Sscanf(cidr, "%d.%d.%d.%d/32", &a, &b, &c, &d); err != nil || n != 4 {
		return 0, fmt.Errorf("invalid CIDR %q", cidr)
	}
	return d, nil
}

func (r *LabGroupClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroupClient{}).
		Complete(r)
}
