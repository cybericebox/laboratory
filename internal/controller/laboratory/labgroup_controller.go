package laboratory

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/cybericebox/laboratory/pkg/netutil"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// labSubnetPrefixLen is the prefix length of per-lab and per-client subnets within VPNBaseNetwork.
const labSubnetPrefixLen = 24

// LabGroupReconciler reconciles a LabGroup object.
type LabGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// PublicVPNEndpoint is the publicly reachable host:port that clients dial
	// (host of the WireGuard demux). Written verbatim to LabGroup.Status.VPN.Endpoint.
	PublicVPNEndpoint string
	// VPNServicePort is the UDP port the VPN server listens on. Defaults to 51820.
	VPNServicePort int
	// VPNBaseNetwork is the base address space for all VPN subnets (e.g. "10.8.0.0/10").
	VPNBaseNetwork string
	// InetBaseNetwork is the base address space for per-lab internet/gateway subnets (e.g. "10.9.0.0/10").
	InetBaseNetwork string
	// DHCPDNS is the DNS server address advertised to WireGuard clients (optional).
	DHCPDNS string
	// VPNImage is the container image for VPN pods.
	VPNImage string
	// GatewayImage is the container image for gateway pods.
	GatewayImage string
	// LabNodeSelector is applied to VPN and gateway pod specs.
	LabNodeSelector map[string]string
	// LabTolerations is applied to VPN and gateway pod specs.
	LabTolerations []corev1.Toleration
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;services;serviceaccounts;endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=allocation.cybericebox.com,resources=pools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=allocation.cybericebox.com,resources=pools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *LabGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("reconciling LabGroup", "name", req.Name)

	var lg laboratoryv1alpha1.LabGroup
	if err := r.Get(ctx, req.NamespacedName, &lg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lg.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lg)
	}

	if !controllerutil.ContainsFinalizer(&lg, names.FinalizerLabGroup) {
		controllerutil.AddFinalizer(&lg, names.FinalizerLabGroup)
		if err := r.Update(ctx, &lg); err != nil {
			return ctrl.Result{}, err
		}
	}

	ns := laboratoryv1alpha1.LabGroupNamespace(lg.Name)

	if err := r.ensureNamespace(ctx, ns, &lg); err != nil {
		logger.Error(err, "ensure namespace")
		return ctrl.Result{}, err
	}

	pubKey, secretName, err := r.ensureVPNKeypair(ctx, ns, &lg)
	if err != nil {
		logger.Error(err, "ensure VPN keypair")
		return ctrl.Result{}, err
	}

	// Reject duplicate routing key — demux uses LabGroup.status.vpn.publicKey
	// as the per-group identity; a collision would silently merge two groups.
	if dup, dupErr := r.findDuplicatePubKey(ctx, pubKey, lg.Name); dupErr != nil {
		return ctrl.Result{}, dupErr
	} else if dup != "" {
		lg.Status.Phase = laboratoryv1alpha1.PhaseFailed
		_ = r.Status().Update(ctx, &lg)
		logger.Error(fmt.Errorf("pubkey collision with LabGroup %q", dup),
			"refusing to register duplicate VPN public key", "labgroup", lg.Name)
		return ctrl.Result{}, nil
	}

	if err = r.ensureVPNService(ctx, ns); err != nil {
		logger.Error(err, "ensure VPN service")
		return ctrl.Result{}, err
	}

	if err = r.ensureServiceAccount(ctx, ns, "vpn"); err != nil {
		logger.Error(err, "ensure VPN service account")
		return ctrl.Result{}, err
	}
	if err = r.ensureRoleBinding(ctx, ns, "vpn", names.RoleVPNName); err != nil {
		logger.Error(err, "ensure VPN role binding")
		return ctrl.Result{}, err
	}
	if err = r.ensureVPNDeployment(ctx, ns); err != nil {
		logger.Error(err, "ensure VPN deployment")
		return ctrl.Result{}, err
	}

	if err = r.ensureServiceAccount(ctx, ns, "gateway"); err != nil {
		logger.Error(err, "ensure gateway service account")
		return ctrl.Result{}, err
	}
	if err = r.ensureRoleBinding(ctx, ns, "gateway", names.RoleGatewayName); err != nil {
		logger.Error(err, "ensure gateway role binding")
		return ctrl.Result{}, err
	}
	if err = r.ensureGatewayDeployment(ctx, ns); err != nil {
		logger.Error(err, "ensure gateway deployment")
		return ctrl.Result{}, err
	}

	if err = r.ensurePool(ctx, ns, names.PoolVPNClients, poolpkg.PoolTypeVPNClients, 0, 254); err != nil {
		logger.Error(err, "ensure vpn-clients pool")
		return ctrl.Result{}, err
	}

	if err = r.ensurePool(ctx, ns, names.PoolLabSubnets, poolpkg.PoolTypeLabSubnets, 1, 254); err != nil {
		logger.Error(err, "ensure lab-subnets pool")
		return ctrl.Result{}, err
	}

	vpnReady, err := r.vpnReadyState(ctx, ns)
	if err != nil {
		logger.Error(err, "check VPN readiness")
		return ctrl.Result{}, err
	}

	lg.Status.Phase = laboratoryv1alpha1.PhaseReady
	lg.Status.Namespace = ns
	lg.Status.VPN.PublicKey = pubKey
	lg.Status.VPN.SecretRef = fmt.Sprintf("%s/%s", ns, secretName)
	lg.Status.VPN.Endpoint = r.PublicVPNEndpoint
	lg.Status.VPN.Registered = vpnReady
	if err = r.Status().Update(ctx, &lg); err != nil {
		return ctrl.Result{}, err
	}

	if !vpnReady {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// findDuplicatePubKey returns the name of another LabGroup that already
// publishes the same pubKey, or "" if pubKey is unique. The reconciler is
// serialised (MaxConcurrentReconciles=1 + leader election), so the
// list-then-write window is safe.
func (r *LabGroupReconciler) findDuplicatePubKey(ctx context.Context, pubKey, self string) (string, error) {
	if pubKey == "" {
		return "", nil
	}
	var list laboratoryv1alpha1.LabGroupList
	if err := r.List(ctx, &list); err != nil {
		return "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == self {
			continue
		}
		if other.Status.VPN.PublicKey == pubKey {
			return other.Name, nil
		}
	}
	return "", nil
}

func (r *LabGroupReconciler) vpnPort() int32 {
	if r.VPNServicePort > 0 {
		return int32(r.VPNServicePort)
	}
	return 51820
}

// vpnReadyState returns true when the VPN Deployment has at least one ready replica.
func (r *LabGroupReconciler) vpnReadyState(ctx context.Context, ns string) (bool, error) {
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &dep); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return dep.Status.ReadyReplicas > 0, nil
}

func (r *LabGroupReconciler) reconcileDelete(ctx context.Context, lg *laboratoryv1alpha1.LabGroup) (ctrl.Result, error) {
	ns := laboratoryv1alpha1.LabGroupNamespace(lg.Name)
	var namespace corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, &namespace); err != nil {
		if errors.IsNotFound(err) {
			controllerutil.RemoveFinalizer(lg, names.FinalizerLabGroup)
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
			Labels: map[string]string{names.LabelGroup: owner.Name},
		},
	}
	return r.Create(ctx, n)
}

func (r *LabGroupReconciler) ensureVPNKeypair(ctx context.Context, ns string, lg *laboratoryv1alpha1.LabGroup) (pubKey, secretName string, err error) {
	secretName = names.SecretVPNKeypair

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

func (r *LabGroupReconciler) ensureVPNService(ctx context.Context, ns string) error {
	var existing corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: ns},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None", // headless — DNS returns pod IP directly, no ClusterIP NAT
			Selector:  map[string]string{"app": "vpn"},
			Ports: []corev1.ServicePort{{
				Name:     "wireguard",
				Protocol: corev1.ProtocolUDP,
				Port:     r.vpnPort(),
			}},
		},
	}
	return r.Create(ctx, svc)
}

func (r *LabGroupReconciler) ensureVPNDeployment(ctx context.Context, ns string) error {
	var existing appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	clientSubnet, err := netutil.SubnetForIndex(r.VPNBaseNetwork, labSubnetPrefixLen, 0)
	if err != nil {
		return fmt.Errorf("derive VPN client subnet: %w", err)
	}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "vpn"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "vpn"},
					Annotations: map[string]string{names.AnnotationNetworks: "eth0@" + names.DefaultNetworkValue},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "vpn",
					NodeSelector:       r.LabNodeSelector,
					Tolerations:        r.LabTolerations,
					Containers: []corev1.Container{{
						Name:            "vpn",
						Image:           r.VPNImage,
						Command:         []string{"/vpn"},
						ImagePullPolicy: corev1.PullIfNotPresent,
						SecurityContext: &corev1.SecurityContext{
							Capabilities: &corev1.Capabilities{
								Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "PRIVATE_KEY",
								ValueFrom: &corev1.EnvVarSource{
									SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: names.SecretVPNKeypair},
										Key:                  "privateKey",
									},
								},
							},
							{Name: "NAMESPACE", Value: ns},
							{Name: "CLIENT_SUBNET", Value: clientSubnet},
							{Name: "VPN_BASE_NETWORK", Value: r.VPNBaseNetwork},
							{Name: "LISTEN_PORT", Value: fmt.Sprint(r.vpnPort())},
							{Name: "DHCP_DNS", Value: r.DHCPDNS},
						},
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
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "gateway"},
					Annotations: map[string]string{names.AnnotationNetworks: "eth0@" + names.DefaultNetworkValue},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "gateway",
					NodeSelector:       r.LabNodeSelector,
					Tolerations:        r.LabTolerations,
					Containers: []corev1.Container{{
						Name:            "gateway",
						Image:           r.GatewayImage,
						Command:         []string{"/gateway"},
						ImagePullPolicy: corev1.PullIfNotPresent,
						SecurityContext: &corev1.SecurityContext{
							Capabilities: &corev1.Capabilities{
								Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
							},
						},
						Env: []corev1.EnvVar{
							{Name: "NAMESPACE", Value: ns},
							{Name: "INET_BASE_NETWORK", Value: r.InetBaseNetwork},
							{Name: "DHCP_DNS", Value: r.DHCPDNS},
						},
					}},
				},
			},
		},
	}
	return r.Create(ctx, d)
}

func (r *LabGroupReconciler) ensureServiceAccount(ctx context.Context, ns, name string) error {
	var existing corev1.ServiceAccount
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	return r.Create(ctx, sa)
}

func (r *LabGroupReconciler) ensureRoleBinding(ctx context.Context, ns, saName, clusterRoleName string) error {
	bindingName := fmt.Sprintf("%s-binding", saName)
	var existing rbacv1.RoleBinding
	if err := r.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: ns},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: ns,
		}},
	}
	return r.Create(ctx, rb)
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
	// Re-reconcile LabGroup when its VPN Pod changes (ready, restart, new IP).
	vpnPodMap := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pod, ok := obj.(*corev1.Pod)
		if !ok || pod.Labels["app"] != "vpn" {
			return nil
		}
		var nsObj corev1.Namespace
		if err := r.Get(ctx, types.NamespacedName{Name: pod.Namespace}, &nsObj); err != nil {
			return nil
		}
		owner := nsObj.Labels[names.LabelGroup]
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
