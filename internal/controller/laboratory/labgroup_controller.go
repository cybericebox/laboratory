package laboratory

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const finalizerLabGroup = "cybericebox.com/labgroup"

// LabGroupReconciler reconciles a LabGroup object.
type LabGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// PublicVPNEndpoint is the publicly reachable host:port that clients dial
	// (host of the WireGuard demux). Written verbatim to LabGroup.Status.VPN.Endpoint.
	PublicVPNEndpoint string
}

const vpnListenPort = 51820

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
		return r.reconcileDelete(ctx, &lg)
	}

	if !controllerutil.ContainsFinalizer(&lg, finalizerLabGroup) {
		controllerutil.AddFinalizer(&lg, finalizerLabGroup)
		if err := r.Update(ctx, &lg); err != nil {
			return ctrl.Result{}, err
		}
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

	if err = r.ensureGatewayDeployment(ctx, ns); err != nil {
		logger.Error(err, "ensure gateway deployment")
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

	backend, backendReady, err := r.discoverVPNBackend(ctx, ns)
	if err != nil {
		logger.Error(err, "discover VPN backend")
		return ctrl.Result{}, err
	}

	lg.Status.Phase = laboratoryv1alpha1.PhaseReady
	lg.Status.Namespace = ns
	lg.Status.VPN.PublicKey = pubKey
	lg.Status.VPN.SecretRef = fmt.Sprintf("%s/%s", ns, secretName)
	lg.Status.VPN.Endpoint = r.PublicVPNEndpoint
	lg.Status.VPN.Backend = backend
	lg.Status.VPN.Registered = backendReady
	if err = r.Status().Update(ctx, &lg); err != nil {
		return ctrl.Result{}, err
	}

	// Re-reconcile soon while waiting for the VPN pod to become Running so
	// Backend gets populated without a Pod-watch trigger.
	if !backendReady {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// discoverVPNBackend returns the podIP:port of a Running VPN pod in ns, or
// ("", false, nil) if no pod is ready yet.
func (r *LabGroupReconciler) discoverVPNBackend(ctx context.Context, ns string) (string, bool, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{"app": "vpn"}); err != nil {
		return "", false, err
	}
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			return fmt.Sprintf("%s:%d", p.Status.PodIP, vpnListenPort), true, nil
		}
	}
	return "", false, nil
}

func (r *LabGroupReconciler) reconcileDelete(ctx context.Context, lg *laboratoryv1alpha1.LabGroup) (ctrl.Result, error) {
	ns := fmt.Sprintf("labgroup-%s", lg.UID)
	var namespace corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, &namespace); err != nil {
		if errors.IsNotFound(err) {
			controllerutil.RemoveFinalizer(lg, finalizerLabGroup)
			return ctrl.Result{}, r.Update(ctx, lg)
		}
		return ctrl.Result{}, err
	}
	if namespace.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &namespace); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, nil, err
	}
	return []byte(key.String()), []byte(key.PublicKey().String()), nil
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

// ensureGatewayDeployment creates the per-LabGroup internet-gateway pod.
// One replica per group, namespace-scoped, mirrors the VPN deployment shape so
// node-agent's LabIfaceReconciler attaches a gw-<labname> OVS port into its
// netns for each lab with Spec.Internet.Enabled.
func (r *LabGroupReconciler) ensureGatewayDeployment(ctx context.Context, ns string) error {
	var existing appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "gateway"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "gateway",
						Image: "cybericebox/gateway:latest",
						Env: []corev1.EnvVar{
							{Name: "NAMESPACE", Value: ns},
						},
					}},
				},
			},
		},
	}
	return r.Create(ctx, d)
}

// ensurePool creates pool "{name}-0" if it doesn't exist, with the allocator-compatible naming
// and bitmap initialised (bit 0 reserved when offset == 0).
func (r *LabGroupReconciler) ensurePool(ctx context.Context, ns, name, poolType string, offset, size uint) error {
	poolName := fmt.Sprintf("%s-0", name)
	var existing allocationv1alpha1.Pool
	if err := r.Get(ctx, types.NamespacedName{Name: poolName, Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	bitmapStr, free := poolpkg.InitBitmap(size, offset)
	p := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: ns,
			Labels: map[string]string{
				poolpkg.PoolTypeLabel:   poolType,
				poolpkg.PoolStateLabel:  poolpkg.PoolStateEmpty,
				poolpkg.PoolGroupLabel:  name,
				poolpkg.LatestPoolLabel: "true",
			},
		},
		Spec: allocationv1alpha1.PoolSpec{Size: size, Offset: offset},
	}
	if err := r.Create(ctx, p); err != nil {
		return err
	}
	p.Status.Free = free
	p.Status.BitMap = bitmapStr
	return r.Status().Update(ctx, p)
}

func (r *LabGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Reconcile the owning LabGroup whenever its VPN pod changes phase or IP,
	// so Status.VPN.Backend follows pod rescheduling without polling.
	vpnPodMap := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return nil
		}
		if pod.Labels["app"] != "vpn" {
			return nil
		}
		var nsObj corev1.Namespace
		if err := r.Get(ctx, types.NamespacedName{Name: pod.Namespace}, &nsObj); err != nil {
			return nil
		}
		owner := nsObj.Labels["laboratory.cybericebox.com/group"]
		if owner == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: owner}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroup{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(vpnPodMap)).
		Complete(r)
}
