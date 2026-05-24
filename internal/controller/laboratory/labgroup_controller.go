package laboratory

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// LabGroupReconciler reconciles a LabGroup object.
type LabGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=allocation.cybericebox.com,resources=pools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=allocation.cybericebox.com,resources=pools/status,verbs=get;update;patch

func (r *LabGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var lg laboratoryv1alpha1.LabGroup
	if err := r.Get(ctx, req.NamespacedName, &lg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lg.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	ns := fmt.Sprintf("labgroup-%s", lg.UID)

	if err := r.ensureNamespace(ctx, ns, &lg); err != nil {
		logger.Error(err, "ensure namespace")
		return ctrl.Result{}, err
	}

	pubKey, secretName, err := r.ensureVPNKeypair(ctx, ns, &lg)
	if err != nil {
		logger.Error(err, "ensure VPN keypair")
		return ctrl.Result{}, err
	}

	if err = r.ensureVPNDeployment(ctx, ns); err != nil {
		logger.Error(err, "ensure VPN deployment")
		return ctrl.Result{}, err
	}

	if err = r.ensurePool(ctx, ns, "vpn-clients", poolpkg.PoolTypeVPNClients, 0, 254); err != nil {
		logger.Error(err, "ensure vpn-clients pool")
		return ctrl.Result{}, err
	}

	if err = r.ensurePool(ctx, ns, "lab-subnets", poolpkg.PoolTypeLabSubnets, 1, 254); err != nil {
		logger.Error(err, "ensure lab-subnets pool")
		return ctrl.Result{}, err
	}

	lg.Status.Phase = laboratoryv1alpha1.PhaseReady
	lg.Status.Namespace = ns
	lg.Status.VPN.PublicKey = pubKey
	lg.Status.VPN.SecretRef = fmt.Sprintf("%s/%s", ns, secretName)
	if err = r.Status().Update(ctx, &lg); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *LabGroupReconciler) ensureNamespace(ctx context.Context, ns string, owner *laboratoryv1alpha1.LabGroup) error {
	var existing corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	n := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{"laboratory.cybericebox.com/group": owner.Name},
		},
	}
	return r.Create(ctx, n)
}

func (r *LabGroupReconciler) ensureVPNKeypair(ctx context.Context, ns string, lg *laboratoryv1alpha1.LabGroup) (pubKey, secretName string, err error) {
	secretName = "vpn-server-keypair"

	if lg.Spec.VPN.KeypairSecretRef != nil {
		ref := lg.Spec.VPN.KeypairSecretRef
		var s corev1.Secret
		if err = r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &s); err != nil {
			return "", "", fmt.Errorf("get provided keypair secret: %w", err)
		}
		return string(s.Data["publicKey"]), ref.Name, nil
	}

	var existing corev1.Secret
	if err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, &existing); err == nil {
		return string(existing.Data["publicKey"]), secretName, nil
	} else if !errors.IsNotFound(err) {
		return "", "", err
	}

	privKey, pub, genErr := generateWireGuardKeypair()
	if genErr != nil {
		return "", "", genErr
	}

	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"privateKey": privKey, "publicKey": pub},
	}
	if err = r.Create(ctx, s); err != nil {
		return "", "", err
	}
	return string(pub), secretName, nil
}

// generateWireGuardKeypair returns (privateKeyB64, publicKeyB64, error).
// Production code should use golang.zx2c4.com/wireguard/wgctrl/wgtypes for real Curve25519 keys.
func generateWireGuardKeypair() ([]byte, []byte, error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, nil, err
	}
	pub := make([]byte, 32)
	copy(pub, priv) // placeholder — replace with actual Curve25519 scalar multiply
	return []byte(base64.StdEncoding.EncodeToString(priv)),
		[]byte(base64.StdEncoding.EncodeToString(pub)), nil
}

func (r *LabGroupReconciler) ensureVPNDeployment(ctx context.Context, ns string) error {
	var existing appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "vpn"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "vpn"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "vpn",
						Image: "cybericebox/vpn:latest",
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "vpn-server-keypair"},
							},
						}},
					}},
				},
			},
		},
	}
	return r.Create(ctx, d)
}

func (r *LabGroupReconciler) ensurePool(ctx context.Context, ns, name, poolType string, offset, size uint) error {
	var existing allocationv1alpha1.Pool
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	p := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				poolpkg.PoolTypeLabel:  poolType,
				poolpkg.PoolStateLabel: poolpkg.PoolStateEmpty,
				poolpkg.PoolGroupLabel: name,
			},
		},
		Spec: allocationv1alpha1.PoolSpec{Size: size, Offset: offset},
	}
	if err := r.Create(ctx, p); err != nil {
		return err
	}
	p.Status.Free = size
	return r.Status().Update(ctx, p)
}

func (r *LabGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroup{}).
		Complete(r)
}
