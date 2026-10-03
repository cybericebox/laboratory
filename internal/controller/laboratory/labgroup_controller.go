package laboratory

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/egress"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/cybericebox/laboratory/pkg/netutil"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// labSubnetPrefixLen is the prefix length of per-lab and per-client subnets within VPNBaseNetwork.
const labSubnetPrefixLen = 24

// LabGroupReconciler reconciles a LabGroup object.
type LabGroupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// PublicVPNEndpoint is the publicly reachable host:port that clients dial
	// (host of the WireGuard demux). Written verbatim to LabGroup.Status.VPN.Endpoint.
	PublicVPNEndpoint string
	// VPNServicePort is the UDP port the VPN server listens on. Defaults to 51820.
	VPNServicePort int
	// VPNBaseNetwork is the base address space for all VPN subnets (e.g. "10.8.0.0/10").
	VPNBaseNetwork string
	// InetBaseNetwork is the base address space for per-lab internet/gateway subnets (e.g. "10.9.0.0/10").
	InetBaseNetwork string
	// VPNImage is the container image for VPN pods.
	VPNImage string
	// GatewayImage is the container image for gateway pods.
	GatewayImage string
	// GroupPods are the resources of the VPN and gateway pods of a NEW group (requests = limits);
	// the Deployments of existing groups are left as they are, so a live event keeps its VPN.
	GroupPods grouppods.Config
	// PriorityClass is the class of the VPN and gateway pods (above the devices). Empty = none.
	PriorityClass string
	// SchedulerName is the kube-scheduler profile of the VPN and gateway pods (bin-packing); empty = the default scheduler.
	SchedulerName string
	// Mirror rewrites the VPN and gateway images for the image cache when their
	// pods are created; the zero value (cache off) rewrites nothing.
	Mirror imagecache.Rewriter
	// Resolver pins those images to a digest, once, at creation; nil pins nothing.
	Resolver imagecache.Resolver

	pinMu       sync.Mutex
	pinFailures map[string][]string
	// rollout lets one group at a time have its VPN and gateway pods replaced because their image, command or resources
	// differ from the configured ones (see rollout.go).
	rollout rolloutGuard
	// SupportEmail is the contact address shown on the VPN probe page.
	SupportEmail string
	// LabNodeSelector is applied to VPN and gateway pod specs.
	LabNodeSelector map[string]string
	// LabTolerations is applied to VPN and gateway pod specs.
	LabTolerations []corev1.Toleration
	// AgentEnabled gates creation of the management-agent RoleBinding in each
	// group namespace. AgentSA identifies the agent ServiceAccount to bind.
	AgentEnabled bool
	AgentSA      types.NamespacedName
	// ProxyEnabled gates the RoleBinding of the L7 proxy's traffic reports in each group namespace.
	ProxyEnabled bool
	// NetworkPolicyEnabled gates creation of the default-deny NetworkPolicy
	// baseline in each group namespace.
	NetworkPolicyEnabled bool
	// OperatorSA is the operator's own ServiceAccount: it is bound to the working role in each group namespace
	// (empty: no binding, for tests).
	OperatorSA types.NamespacedName
	// VPNStatsInterval is STATS_INTERVAL of the VPN pod of a new group; zero leaves the pod's own default.
	VPNStatsInterval time.Duration
	// ImagePullSecrets names registry Secrets of the operator namespace. They are
	// copied into every group namespace and referenced by the VPN and gateway pods.
	ImagePullSecrets []string
	// Scheduled makes the group's VPN and gateway pods wait for the scheduler to
	// dispatch them (see labgroup_sched.go). Off: they start as soon as the group is created.
	Scheduled bool
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;services;serviceaccounts;endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=allocation.cybericebox.com,resources=pools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=allocation.cybericebox.com,resources=pools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=laboratory-agent-role;laboratory-vpn-role;laboratory-operator-namespaced;laboratory-proxy-reports,verbs=bind

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

	ns := laboratoryv1alpha1.LabGroupNamespaceOf(&lg)
	r.rollout.begin(ns)
	clientSubnet, err := netutil.SubnetForIndex(r.VPNBaseNetwork, labSubnetPrefixLen, 0)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("derive VPN client subnet: %w", err)
	}

	if err := r.ensureNamespace(ctx, ns, &lg); err != nil {
		logger.Error(err, "ensure namespace")
		return ctrl.Result{}, err
	}

	if err := r.ensureOperatorBinding(ctx, ns); err != nil {
		logger.Error(err, "ensure the operator's role binding")
		return ctrl.Result{}, err
	}

	if err := copyPullSecrets(ctx, r.Client, r.ImagePullSecrets, ns); err != nil {
		logger.Error(err, "copy image pull secrets")
		return ctrl.Result{}, err
	}
	if err := syncTenantPullSecret(ctx, r.Client, names.TenantOf(lg.Labels), ns); err != nil {
		logger.Error(err, "copy the tenant's image pull secret")
		return ctrl.Result{}, err
	}

	if err := r.ensureDefaultDeny(ctx, ns); err != nil {
		logger.Error(err, "ensure default-deny network policy")
		return ctrl.Result{}, err
	}

	if err := r.ensureGroupDisruptionBudget(ctx, ns); err != nil {
		logger.Error(err, "ensure pod disruption budget")
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
		msg := fmt.Sprintf("VPN public key collides with LabGroup %q; refusing to register", dup)
		r.Recorder.Event(&lg, corev1.EventTypeWarning, labstatus.ReasonPubKeyCollision, msg)
		logger.Error(fmt.Errorf("%s", msg), "refusing to register duplicate VPN public key", "labgroup", lg.Name)
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
	if err = r.ensureGroupScheduling(ctx, &lg); err != nil {
		logger.Error(err, "ensure scheduling state")
		return ctrl.Result{}, err
	}
	// Suspension stops task devices only. Keep the team's tunnel and internet
	// gateway running so participants can test their connection during a pause.
	if !r.groupPodQueued(&lg, "vpn") {
		if err = r.ensureVPNDeployment(ctx, ns, lg.Spec.VPN.Disabled, lg.Spec.VPN.Size); err != nil {
			logger.Error(err, "ensure VPN deployment")
			return ctrl.Result{}, err
		}
	}

	if err = r.ensureServiceAccount(ctx, ns, "gateway"); err != nil {
		logger.Error(err, "ensure gateway service account")
		return ctrl.Result{}, err
	}
	if err = r.ensureRoleBinding(ctx, ns, "gateway", names.RoleGatewayName); err != nil {
		logger.Error(err, "ensure gateway role binding")
		return ctrl.Result{}, err
	}
	if !r.groupPodQueued(&lg, "gateway") {
		if err = r.ensureGatewayDeployment(ctx, ns, false, lg.Spec.Gateway.Size); err != nil {
			logger.Error(err, "ensure gateway deployment")
			return ctrl.Result{}, err
		}
	}

	// An image update waits its turn (one group at a time) and the next group starts when this one's pods are rolled out.
	rolloutRequeue, err := r.settleRollout(ctx, ns)
	if err != nil {
		logger.Error(err, "check the rollout of the VPN and gateway pods")
		return ctrl.Result{}, err
	}
	later := func(res ctrl.Result) ctrl.Result {
		if rolloutRequeue && (res.RequeueAfter == 0 || res.RequeueAfter > rolloutPoll) {
			res.RequeueAfter = rolloutPoll
		}
		return res
	}

	if err = r.syncPodLabels(ctx, &lg); err != nil {
		logger.Error(err, "sync pod labels")
		return ctrl.Result{}, err
	}

	if err = r.ensureVPNGatewayPolicies(ctx, ns); err != nil {
		logger.Error(err, "ensure vpn/gateway CiliumNetworkPolicies")
		return ctrl.Result{}, err
	}

	if err = r.ensureAgentRoleBinding(ctx, ns); err != nil {
		logger.Error(err, "ensure agent role binding")
		return ctrl.Result{}, err
	}
	if err = r.ensureProxyReportsBinding(ctx, ns); err != nil {
		logger.Error(err, "ensure proxy reports role binding")
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

	if lg.Spec.Suspended {
		vpnReady := false
		if !lg.Spec.VPN.Disabled {
			vpnReady, err = r.vpnReadyState(ctx, ns)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		r.reportPinWarning(&lg, ns)
		lg.Status.Phase = laboratoryv1alpha1.PhaseSuspended
		lg.Status.Namespace = ns
		lg.Status.Suspended = true
		lg.Status.VPN.PublicKey = pubKey
		lg.Status.VPN.SecretRef = fmt.Sprintf("%s/%s", ns, secretName)
		lg.Status.VPN.Endpoint = r.PublicVPNEndpoint
		lg.Status.VPN.ClientSubnet = clientSubnet
		lg.Status.VPN.Registered = vpnReady
		if err = r.Status().Update(ctx, &lg); err != nil {
			return ctrl.Result{}, err
		}
		if !lg.Spec.VPN.Disabled && !vpnReady {
			return later(ctrl.Result{RequeueAfter: 5 * time.Second}), nil
		}
		return later(ctrl.Result{}), nil
	}

	vpnReady := false
	if !lg.Spec.VPN.Disabled {
		vpnReady, err = r.vpnReadyState(ctx, ns)
		if err != nil {
			logger.Error(err, "check VPN readiness")
			return ctrl.Result{}, err
		}
	}

	r.reportPinWarning(&lg, ns)
	lg.Status.Phase = laboratoryv1alpha1.PhaseReady
	if r.groupPodQueued(&lg, "vpn") || r.groupPodQueued(&lg, "gateway") {
		lg.Status.Phase = laboratoryv1alpha1.PhaseQueued
	}
	lg.Status.Namespace = ns
	lg.Status.Suspended = false
	lg.Status.VPN.PublicKey = pubKey
	lg.Status.VPN.SecretRef = fmt.Sprintf("%s/%s", ns, secretName)
	lg.Status.VPN.Endpoint = r.PublicVPNEndpoint
	lg.Status.VPN.ClientSubnet = clientSubnet
	wasRegistered := lg.Status.VPN.Registered
	lg.Status.VPN.Registered = vpnReady
	if err = r.Status().Update(ctx, &lg); err != nil {
		return ctrl.Result{}, err
	}

	if !vpnReady && !lg.Spec.VPN.Disabled {
		if wasRegistered {
			r.Recorder.Event(&lg, corev1.EventTypeWarning, labstatus.ReasonWaitingForVPNServer,
				"VPN server has no ready replicas; group not registered in demux")
		}
		return later(ctrl.Result{RequeueAfter: 5 * time.Second}), nil
	}
	if !wasRegistered {
		r.Recorder.Event(&lg, corev1.EventTypeNormal, labstatus.ReasonReady, "VPN server registered in demux")
	}
	return later(ctrl.Result{}), nil
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
	ns := laboratoryv1alpha1.LabGroupNamespaceOf(lg)

	// Never touch a namespace this group did not create: one that exists without our label (a system namespace, another
	// group's, anything made by hand) is left exactly as it is, and the group is simply let go.
	var existing corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, &existing); err != nil {
		if errors.IsNotFound(err) {
			controllerutil.RemoveFinalizer(lg, names.FinalizerLabGroup)
			return ctrl.Result{}, r.Update(ctx, lg)
		}
		return ctrl.Result{}, err
	}
	if !ownsNamespace(&existing, lg) {
		ctrl.LoggerFrom(ctx).Info("the namespace of the group is not the group's: leaving it alone", "namespace", ns)
		if r.Recorder != nil {
			r.Recorder.Eventf(lg, corev1.EventTypeWarning, "ForeignNamespace", "namespace %s does not carry this group's label: left untouched", ns)
		}
		controllerutil.RemoveFinalizer(lg, names.FinalizerLabGroup)
		return ctrl.Result{}, r.Update(ctx, lg)
	}

	// Drain Labs and LabGroupClients BEFORE deleting the namespace. Both carry
	// finalizers cleaned up by the per-group VPN/gateway pods; deleting the
	// namespace first would remove those Deployments before the finalizers
	// cleared, hanging the namespace in Terminating forever. Draining them while
	// the pods still run lets that graceful cleanup happen.
	drained, err := r.drainGroupWorkloads(ctx, ns)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !drained {
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

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

// drainGroupWorkloads deletes every Lab and LabGroupClient in the group
// namespace and reports whether they are all gone. Called before namespace
// teardown so the still-running VPN/gateway pods clear their finalizers.
func (r *LabGroupReconciler) drainGroupWorkloads(ctx context.Context, ns string) (bool, error) {
	var labs laboratoryv1alpha1.LabList
	if err := r.List(ctx, &labs, client.InNamespace(ns)); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	for i := range labs.Items {
		if labs.Items[i].DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &labs.Items[i]); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
	}

	var clients laboratoryv1alpha1.LabGroupClientList
	if err := r.List(ctx, &clients, client.InNamespace(ns)); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	for i := range clients.Items {
		if clients.Items[i].DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &clients.Items[i]); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
	}

	return len(labs.Items) == 0 && len(clients.Items) == 0, nil
}

// ownsNamespace says whether the namespace was made for this group: it carries the group's label. The operator puts the
// label on every namespace it creates and works only in labelled ones (the admission policy says the same).
func ownsNamespace(ns *corev1.Namespace, lg *laboratoryv1alpha1.LabGroup) bool {
	return ns.Labels[names.LabelGroup] == lg.Name
}

// errForeignNamespace is a namespace that exists but is not this group's.
var errForeignNamespace = fmt.Errorf("the namespace exists and is not this group's")

// ensureNamespace creates the namespace of a group, labelled with the group. It never adopts a namespace that exists
// without the group's label: a tenant picks the group name, and must not be able to point the operator at kube-system
// or at another group's namespace.
func (r *LabGroupReconciler) ensureNamespace(ctx context.Context, ns string, owner *laboratoryv1alpha1.LabGroup) error {
	var existing corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, &existing); err == nil {
		if !ownsNamespace(&existing, owner) {
			return fmt.Errorf("%w: %s", errForeignNamespace, ns)
		}
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

// systemPodsConverged says whether every pod of the group's own component (the VPN or the gateway) carries
// LabelComponent. Pods made before the label existed do not, until the Deployment's template change has rolled them; a
// selector that needs the label (Service, network policy) waits for that so an upgrade never leaves a gap. A device pod
// that happens to be named like the component carries the lab label and is not counted.
func (r *LabGroupReconciler) systemPodsConverged(ctx context.Context, ns, component string) (bool, error) {
	noLab, err := labels.NewRequirement(names.LabelLab, selection.DoesNotExist, nil)
	if err != nil {
		return false, err
	}
	noComponent, err := labels.NewRequirement(names.LabelComponent, selection.DoesNotExist, nil)
	if err != nil {
		return false, err
	}
	sel := labels.NewSelector().Add(*noLab, *noComponent)
	req, err := labels.NewRequirement("app", selection.Equals, []string{component})
	if err != nil {
		return false, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel.Add(*req)}); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			return false, nil
		}
	}
	return true, nil
}

// systemSelector is the label set that selects the group's VPN or gateway pods: LabelComponent once the pods carry it,
// the old `app` label until they do.
func (r *LabGroupReconciler) systemSelector(ctx context.Context, ns, component string) (map[string]string, error) {
	ok, err := r.systemPodsConverged(ctx, ns, component)
	if err != nil {
		return nil, err
	}
	if ok {
		return map[string]string{names.LabelComponent: component}, nil
	}
	return map[string]string{"app": component}, nil
}

func (r *LabGroupReconciler) ensureVPNService(ctx context.Context, ns string) error {
	selector, err := r.systemSelector(ctx, ns, names.ComponentVPN)
	if err != nil {
		return err
	}
	var existing corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &existing); err == nil {
		if !maps.Equal(existing.Spec.Selector, selector) {
			existing.Spec.Selector = selector
			return r.Update(ctx, &existing)
		}
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: ns},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None", // headless — DNS returns pod IP directly, no ClusterIP NAT
			Selector:  selector,
			Ports: []corev1.ServicePort{{
				Name:     "wireguard",
				Protocol: corev1.ProtocolUDP,
				Port:     r.vpnPort(),
			}},
		},
	}
	return r.Create(ctx, svc)
}

// ensureVPNDeployment creates the VPN pod at the size the group's spec says (the chart's default without one); an existing one keeps
// the size it has: a group's pods are never resized.
func (r *LabGroupReconciler) ensureVPNDeployment(ctx context.Context, ns string, suspended bool, size *laboratoryv1alpha1.GroupPodSize) error {
	replicas := int32(1)
	if suspended {
		replicas = 0
	}
	var existing appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &existing); err == nil {
		changed := existing.Spec.Replicas == nil || *existing.Spec.Replicas != replicas
		existing.Spec.Replicas = ptrInt32(replicas)
		// The configured image reaches the VPN pods that already run, one group at a time (a rolling update); their size stays.
		if r.convergeGroupPod(ctx, ns, &existing, "vpn", r.VPNImage) {
			changed = true
		}
		// The hardened shape reaches the VPN pods that already run too (a rolling restart of the pod).
		if hardenGroupPod(&existing.Spec.Template.Spec, "vpn", vpnCaps) {
			changed = true
		}
		if setComponentLabel(&existing.Spec.Template, names.ComponentVPN) {
			changed = true
		}
		if existing.Spec.Template.Annotations[names.AnnotationConntrackAccounting] != "true" {
			if existing.Spec.Template.Annotations == nil {
				existing.Spec.Template.Annotations = map[string]string{}
			}
			existing.Spec.Template.Annotations[names.AnnotationConntrackAccounting] = "true"
			changed = true
		}
		if len(existing.Spec.Template.Spec.Containers) > 0 {
			container := &existing.Spec.Template.Spec.Containers[0]
			found := false
			for i := range container.Env {
				if container.Env[i].Name == "SUPPORT_EMAIL" {
					found = true
					if container.Env[i].Value != r.SupportEmail {
						container.Env[i].Value = r.SupportEmail
						changed = true
					}
					break
				}
			}
			if !found {
				container.Env = append(container.Env, corev1.EnvVar{Name: "SUPPORT_EMAIL", Value: r.SupportEmail})
				changed = true
			}
		}
		if changed {
			return r.Update(ctx, &existing)
		}
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	vpnImage := r.cachedImage(ctx, ns, r.VPNImage)
	clientSubnet, err := netutil.SubnetForIndex(r.VPNBaseNetwork, labSubnetPrefixLen, 0)
	if err != nil {
		return fmt.Errorf("derive VPN client subnet: %w", err)
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "vpn"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "vpn", names.LabelComponent: names.ComponentVPN},
					Annotations: map[string]string{names.AnnotationDefaultNetwork: "eth0", names.AnnotationConntrackAccounting: "true"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "vpn",
					PriorityClassName:  r.PriorityClass,
					SchedulerName:      r.SchedulerName,
					ImagePullSecrets:   pullSecretRefs(r.ImagePullSecrets),
					NodeSelector:       r.LabNodeSelector,
					Tolerations:        r.LabTolerations,
					Containers: []corev1.Container{{
						Name:            "vpn",
						Resources:       r.GroupPods.VPNFor(size),
						Image:           vpnImage,
						Command:         []string{"/lab", "vpn"},
						ImagePullPolicy: pullPolicyFor(vpnImage),
						Env: append([]corev1.EnvVar{
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
							{Name: "SUPPORT_EMAIL", Value: r.SupportEmail},
						}, r.vpnStatsEnv()...),
					}},
				},
			},
		},
	}
	hardenGroupPod(&d.Spec.Template.Spec, "vpn", vpnCaps)
	return r.Create(ctx, d)
}

// ensureGatewayDeployment creates the per-LabGroup internet-gateway pod.
// One replica per group, namespace-scoped, mirrors the VPN deployment shape so
// node-agent's LabIfaceReconciler attaches a gw-<labname> OVS port into its
// netns for each lab with Spec.Internet.Enabled.
func (r *LabGroupReconciler) ensureGatewayDeployment(ctx context.Context, ns string, suspended bool, size *laboratoryv1alpha1.GroupPodSize) error {
	replicas := int32(1)
	if suspended {
		replicas = 0
	}
	var existing appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: ns}, &existing); err == nil {
		changed := existing.Spec.Replicas == nil || *existing.Spec.Replicas != replicas
		existing.Spec.Replicas = ptrInt32(replicas)
		// The configured image reaches the gateways that already run, one group at a time (a rolling update); their size stays.
		if r.convergeGroupPod(ctx, ns, &existing, "gateway", r.GatewayImage) {
			changed = true
		}
		// A security setting reaches the gateways that already run too (a restart of the pod).
		if r.convergeGateway(&existing) {
			changed = true
		}
		if setComponentLabel(&existing.Spec.Template, names.ComponentGateway) {
			changed = true
		}
		if hardenGroupPod(&existing.Spec.Template.Spec, "gateway", gatewayCaps) {
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Update(ctx, &existing)
	} else if !errors.IsNotFound(err) {
		return err
	}
	gatewayImage := r.cachedImage(ctx, ns, r.GatewayImage)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "gateway", names.LabelComponent: names.ComponentGateway},
					Annotations: map[string]string{names.AnnotationDefaultNetwork: "eth0"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "gateway",
					PriorityClassName:  r.PriorityClass,
					SchedulerName:      r.SchedulerName,
					ImagePullSecrets:   pullSecretRefs(r.ImagePullSecrets),
					NodeSelector:       r.LabNodeSelector,
					Tolerations:        r.LabTolerations,
					Containers: []corev1.Container{{
						Name:            "gateway",
						Resources:       r.GroupPods.GatewayFor(size),
						Image:           gatewayImage,
						Command:         []string{"/lab", "gateway"},
						ImagePullPolicy: pullPolicyFor(gatewayImage),
						Env:             r.gatewayEnv(ns),
					}},
				},
			},
		},
	}
	hardenGroupPod(&d.Spec.Template.Spec, "gateway", gatewayCaps)
	return r.Create(ctx, d)
}

// setComponentLabel puts LabelComponent on a pod template; it reports whether it changed anything. The Deployment's
// selector stays as it was (it is immutable), only the pods' labels grow.
func setComponentLabel(t *corev1.PodTemplateSpec, component string) bool {
	if t.Labels[names.LabelComponent] == component {
		return false
	}
	if t.Labels == nil {
		t.Labels = map[string]string{}
	}
	t.Labels[names.LabelComponent] = component
	return true
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

// ensureAgentRoleBinding creates the RoleBinding granting the management-agent
// ServiceAccount access to a LabGroup namespace, gated by r.AgentEnabled. It is
// a no-op when disabled, and idempotent when the RoleBinding already exists.
func (r *LabGroupReconciler) ensureAgentRoleBinding(ctx context.Context, ns string) error {
	if !r.AgentEnabled {
		return nil
	}
	var existing rbacv1.RoleBinding
	if err := r.Get(ctx, types.NamespacedName{Name: names.AgentRoleBindingName, Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.AgentRoleBindingName, Namespace: ns},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     names.RoleAgentName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      r.AgentSA.Name,
			Namespace: r.AgentSA.Namespace,
		}},
	}
	return r.Create(ctx, rb)
}

// ensureProxyReportsBinding lets the L7 proxy write its LabTrafficReports in this group namespace (and in no other): the right is a
// RoleBinding here, not a cluster-wide permission. A no-op when the proxy is off.
func (r *LabGroupReconciler) ensureProxyReportsBinding(ctx context.Context, ns string) error {
	if !r.ProxyEnabled {
		return nil
	}
	var existing rbacv1.RoleBinding
	if err := r.Get(ctx, types.NamespacedName{Name: names.ProxyReportsBindingName, Namespace: ns}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.ProxyReportsBindingName, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: names.RoleProxyReportsName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "laboratory-proxy", Namespace: names.ProxyNamespace}},
	})
}

// ensureDefaultDeny creates a default-deny NetworkPolicy in the group
// namespace, selecting all pods and denying all ingress and egress traffic by
// default. It is a no-op when r.NetworkPolicyEnabled is false, and idempotent
// otherwise (CreateOrUpdate).
func (r *LabGroupReconciler) ensureDefaultDeny(ctx context.Context, ns string) error {
	if !r.NetworkPolicyEnabled {
		return nil
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(
		ctx, r.Client, np, func() error {
			np.Spec = networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			}
			return nil
		},
	)
	return err
}

// groupDisruptionBudgetName is the PodDisruptionBudget of a lab group namespace.
const groupDisruptionBudgetName = "lab-group"

// ensureGroupDisruptionBudget blocks voluntary evictions (node drains, the
// autoscaler) of every pod in the group namespace: the VPN, the gateway and the
// single-replica device pods all belong to running labs, so a drain must be an
// explicit, force-deleting decision. One budget covers the whole namespace; a
// pod under two budgets cannot be evicted at all, so nothing else may select
// these pods.
func (r *LabGroupReconciler) ensureGroupDisruptionBudget(ctx context.Context, ns string) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: groupDisruptionBudgetName, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		maxUnavailable := intstr.FromInt32(0)
		pdb.Spec.Selector = &metav1.LabelSelector{}
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.MinAvailable = nil
		return nil
	})
	return err
}

// ciliumNetworkPolicyGVK is the GroupVersionKind of Cilium's CiliumNetworkPolicy
// CRD. It has no typed Go struct in this repo (and its CRD is not installed in
// envtest), so policies are built and applied as unstructured.Unstructured.
var ciliumNetworkPolicyGVK = schema.GroupVersionKind{
	Group:   "cilium.io",
	Version: "v2",
	Kind:    "CiliumNetworkPolicy",
}

// matchLabelsOf is a label set as the unstructured form of a selector.
func matchLabelsOf(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// vpnCiliumPolicy builds the CiliumNetworkPolicy locking down the vpn pod's
// egress to kube-apiserver only (its reconciler needs the API; no DNS, no
// world). Ingress allows WireGuard UDP only from the proxy pod: its wg-demux
// container receives every client packet on the shared edge IP and forwards
// it to the group's vpn pod, so that pod (not "world") is the source. Replies
// go back through conntrack.
func vpnCiliumPolicy(ns string, selector map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]interface{}{
				"name":      "vpn-egress",
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"endpointSelector": map[string]interface{}{
					"matchLabels": matchLabelsOf(selector),
				},
				"egress": []interface{}{apiServerEgress()},
				"ingress": []interface{}{
					map[string]interface{}{
						"fromEndpoints": []interface{}{
							map[string]interface{}{
								"matchLabels": map[string]interface{}{
									"k8s:io.kubernetes.pod.namespace": names.ProxyNamespace,
									"app":                             "laboratory-proxy-l7",
								},
							},
						},
						"toPorts": []interface{}{
							map[string]interface{}{
								"ports": []interface{}{
									map[string]interface{}{
										"port":     "51820",
										"protocol": "UDP",
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// apiServerEgress allows the API server entity on its API ports only (not every port of the node it may run on).
func apiServerEgress() map[string]interface{} {
	return map[string]interface{}{
		"toEntities": []interface{}{"kube-apiserver"},
		"toPorts": []interface{}{map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"port": "6443", "protocol": "TCP"},
				map[string]interface{}{"port": "443", "protocol": "TCP"},
			},
		}},
	}
}

// gatewayCiliumPolicy builds the CiliumNetworkPolicy for the gateway pod. Egress: the API server on its API ports
// (its reconciler), and the outside world MINUS the internal ranges (egress.DenyV4 and DenyV6: the metadata service,
// the private ranges, CGNAT, the cluster and node networks). Never in-cluster pods, no DNS. The kube-apiserver entity is
// not a way out for the labs: they leave through this pod, and the gateway's own filter drops what they send to those
// ranges. No ingress rule is needed; replies flow back via conntrack.
func gatewayCiliumPolicy(ns string, selector map[string]string) *unstructured.Unstructured {
	toAny := func(list []string) []interface{} {
		out := make([]interface{}, len(list))
		for i, c := range list {
			out[i] = c
		}
		return out
	}
	egressRules := []interface{}{
		apiServerEgress(),
		map[string]interface{}{"toCIDRSet": []interface{}{
			map[string]interface{}{"cidr": "0.0.0.0/0", "except": toAny(egress.DenyV4)},
			map[string]interface{}{"cidr": "::/0", "except": toAny(egress.DenyV6)},
		}},
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]interface{}{
				"name":      "gateway-egress",
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"endpointSelector": map[string]interface{}{
					"matchLabels": matchLabelsOf(selector),
				},
				"egress": egressRules,
			},
		},
	}
}

// ensureVPNGatewayPolicies applies the vpn-egress and gateway-egress
// CiliumNetworkPolicies in the group namespace, locking down vpn and gateway
// pod egress per vpnCiliumPolicy/gatewayCiliumPolicy. It is a no-op when
// r.NetworkPolicyEnabled is false, and idempotent otherwise (CreateOrUpdate).
//
// CiliumNetworkPolicy has no typed Go struct in this repo and its CRD is not
// installed in envtest, so this applies unstructured.Unstructured objects and
// is exercised in real clusters only; the policy content itself is covered by
// plain-Go unit tests against vpnCiliumPolicy/gatewayCiliumPolicy.
func (r *LabGroupReconciler) ensureVPNGatewayPolicies(ctx context.Context, ns string) error {
	if !r.NetworkPolicyEnabled {
		return nil
	}
	vpnSel, err := r.systemSelector(ctx, ns, names.ComponentVPN)
	if err != nil {
		return err
	}
	gwSel, err := r.systemSelector(ctx, ns, names.ComponentGateway)
	if err != nil {
		return err
	}
	for _, desired := range []*unstructured.Unstructured{vpnCiliumPolicy(ns, vpnSel), gatewayCiliumPolicy(ns, gwSel)} {
		spec, found, err := unstructured.NestedMap(desired.Object, "spec")
		if err != nil || !found {
			return fmt.Errorf("cilium policy %s missing spec", desired.GetName())
		}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(ciliumNetworkPolicyGVK)
		obj.SetName(desired.GetName())
		obj.SetNamespace(ns)

		if _, err = controllerutil.CreateOrUpdate(
			ctx, r.Client, obj, func() error {
				return unstructured.SetNestedMap(obj.Object, spec, "spec")
			},
		); err != nil {
			return fmt.Errorf("apply cilium policy %s: %w", desired.GetName(), err)
		}
	}
	return nil
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
		if !ok || !isVPNPod(pod) {
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

// isVPNPod says whether a pod is the VPN of a group: it carries LabelComponent, or (a pod made before that label) `app=vpn`
// without the lab label of a device pod.
func isVPNPod(pod *corev1.Pod) bool {
	if c := pod.Labels[names.LabelComponent]; c != "" {
		return c == names.ComponentVPN
	}
	_, device := pod.Labels[names.LabelLab]
	return !device && pod.Labels["app"] == names.ComponentVPN
}

// cachedImage is the reference a new VPN or gateway pod of the group pulls:
// through the image cache, pinned to the digest of the tag at this moment. When
// the digest cannot be resolved the tag is used, and the failure is kept for the
// group status (and the log).
func (r *LabGroupReconciler) cachedImage(ctx context.Context, ns, image string) string {
	if r.Mirror.Prefix == "" || r.Mirror.Rewrite(image) == image {
		return image
	}
	digest := ""
	if r.Resolver != nil {
		d, err := r.Resolver.Resolve(ctx, image)
		if err != nil {
			log.FromContext(ctx).Info("image not pinned to a digest, pulled by tag", "image", image, "err", err.Error())
			r.notePinFailure(ns, fmt.Sprintf("%s (%v)", image, err))
		} else {
			digest = d
		}
	}
	return r.Mirror.RewritePinned(image, digest)
}

func (r *LabGroupReconciler) notePinFailure(ns, what string) {
	r.pinMu.Lock()
	defer r.pinMu.Unlock()
	if r.pinFailures == nil {
		r.pinFailures = map[string][]string{}
	}
	r.pinFailures[ns] = append(r.pinFailures[ns], what)
}

// takePinWarning returns, and forgets, the pin failures noted for a group namespace.
func (r *LabGroupReconciler) takePinWarning(ns string) string {
	r.pinMu.Lock()
	defer r.pinMu.Unlock()
	f := r.pinFailures[ns]
	delete(r.pinFailures, ns)
	if len(f) == 0 {
		return ""
	}
	return "images not pinned to a digest, pulled by tag: " + strings.Join(f, "; ")
}

// reportPinWarning copies a fresh pin failure into the group status and raises an event.
func (r *LabGroupReconciler) reportPinWarning(lg *laboratoryv1alpha1.LabGroup, ns string) {
	if w := r.takePinWarning(ns); w != "" {
		lg.Status.ImageWarning = w
		if r.Recorder != nil {
			r.Recorder.Event(lg, corev1.EventTypeWarning, "ImageNotPinned", w)
		}
	}
}

// vpnStatsEnv is STATS_INTERVAL of the VPN pod, when the chart sets one.
func (r *LabGroupReconciler) vpnStatsEnv() []corev1.EnvVar {
	if r.VPNStatsInterval <= 0 {
		return nil
	}
	return []corev1.EnvVar{{Name: "STATS_INTERVAL", Value: r.VPNStatsInterval.String()}}
}

// gatewayEnv is the environment of the gateway container: where it lives and the lab address space.
func (r *LabGroupReconciler) gatewayEnv(ns string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NAMESPACE", Value: ns},
		{Name: "INET_BASE_NETWORK", Value: r.InetBaseNetwork},
	}
}

// convergeGateway brings the security settings of an existing gateway Deployment to the current ones and says
// whether anything changed.
func (r *LabGroupReconciler) convergeGateway(d *appsv1.Deployment) bool {
	changed := false
	for i := range d.Spec.Template.Spec.Containers {
		c := &d.Spec.Template.Spec.Containers[i]
		if c.Name != "gateway" {
			continue
		}
		for _, want := range r.gatewayEnv(d.Namespace) {
			if upsertEnv(c, want) {
				changed = true
			}
		}
		// The egress lists were once environment variables; they are a constant of the gateway now.
		kept := c.Env[:0]
		for _, e := range c.Env {
			if e.Name == "GATEWAY_EGRESS_DENY_CIDRS" || e.Name == "GATEWAY_EGRESS_ALLOW_CIDRS" {
				changed = true
				continue
			}
			kept = append(kept, e)
		}
		c.Env = kept
	}
	return changed
}

// upsertEnv sets a plain env var on the container and says whether that changed it.
func upsertEnv(c *corev1.Container, want corev1.EnvVar) bool {
	for i := range c.Env {
		if c.Env[i].Name == want.Name {
			if c.Env[i].Value == want.Value && c.Env[i].ValueFrom == nil {
				return false
			}
			c.Env[i] = want
			return true
		}
	}
	c.Env = append(c.Env, want)
	return true
}

// ensureOperatorBinding gives the operator its working permissions in a group namespace: a RoleBinding of its own
// ServiceAccount to the namespaced ClusterRole. The operator has no cluster-wide write access (see the chart's
// clusterrole.yaml); this binding, created with its right to create RoleBindings and to `bind` that role, is how
// it acts in the namespaces of its groups and nowhere else.
func (r *LabGroupReconciler) ensureOperatorBinding(ctx context.Context, ns string) error {
	if r.OperatorSA.Name == "" {
		return nil
	}
	want := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: names.RoleOperatorNamespacedName}
	subject := rbacv1.Subject{Kind: "ServiceAccount", Name: r.OperatorSA.Name, Namespace: r.OperatorSA.Namespace}
	var existing rbacv1.RoleBinding
	err := r.Get(ctx, types.NamespacedName{Name: names.OperatorRoleBindingName, Namespace: ns}, &existing)
	if err == nil {
		if len(existing.Subjects) == 1 && existing.Subjects[0] == subject {
			return nil
		}
		existing.Subjects = []rbacv1.Subject{subject}
		return r.Update(ctx, &existing)
	} else if !errors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: names.OperatorRoleBindingName, Namespace: ns},
		RoleRef:    want,
		Subjects:   []rbacv1.Subject{subject},
	})
}
