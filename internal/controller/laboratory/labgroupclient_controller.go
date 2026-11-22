package laboratory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
	labstatus "github.com/cybericebox/laboratory/internal/status"
	"github.com/cybericebox/laboratory/internal/switches"
	"github.com/cybericebox/laboratory/internal/vpn"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

// LabGroupClientReconciler reconciles a LabGroupClient object.
type LabGroupClientReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Reader reads straight from the API server (nil: through Client).
	Reader client.Reader
	// LabBaseNetwork is the whole lab address space, advertised to WireGuard clients as their AllowedIPs (e.g. "10.128.0.0/9").
	LabBaseNetwork string
	// Switches is what the laboratory serves (nil: everything). A client needs the VPN: where it is off the object is refused here, for
	// whatever slipped past the admission policy and the agent.
	Switches *switches.Cluster
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupclients/finalizers,verbs=update
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labgroupaccesspolicies,verbs=get;list;watch
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

func (r *LabGroupClientReconciler) reconcileCreate(
	ctx context.Context,
	lgc *laboratoryv1alpha1.LabGroupClient,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(lgc, names.FinalizerLabGroupClient) {
		controllerutil.AddFinalizer(lgc, names.FinalizerLabGroupClient)
		if err := r.Update(ctx, lgc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if r.Switches != nil && !r.Switches.VPN {
		msg := switches.ReasonVPNUnavailable + ": this laboratory runs no VPN (vpn.enabled is off), so a client has nothing to connect to"
		labstatus.SetReady(&lgc.Status.Conditions, lgc.Generation, false, labstatus.ReasonValidationFailed, msg)
		_ = r.Status().Update(ctx, lgc)
		r.Recorder.Event(lgc, corev1.EventTypeWarning, labstatus.ReasonValidationFailed, msg)
		return ctrl.Result{}, nil
	}

	// The caller generates the WireGuard keypair and provides the public key;
	// the cluster never generates or holds the private key.
	pubKey := lgc.Spec.PublicKey
	if pubKey == "" {
		msg := "spec.publicKey is required — the caller generates the keypair; the cluster never holds the private key"
		labstatus.SetReady(&lgc.Status.Conditions, lgc.Generation, false, "PublicKeyRequired", msg)
		_ = r.Status().Update(ctx, lgc)
		r.Recorder.Event(lgc, corev1.EventTypeWarning, "PublicKeyRequired", msg)
		return ctrl.Result{}, nil
	}
	assignedIP := lgc.Status.AssignedIP

	if assignedIP == "" {

		// Reject duplicate pubkey in the same group — peers are identified by
		// pubkey + assignedIP on the VPN server, so two LGCs with the same key
		// would race for the peer slot.
		if dup, dupErr := r.findDuplicatePubKey(ctx, pubKey, lgc); dupErr != nil {
			return ctrl.Result{}, dupErr
		} else if dup != "" {
			msg := fmt.Sprintf("public key collides with client %q; refusing to allocate", dup)
			labstatus.SetReady(&lgc.Status.Conditions, lgc.Generation, false, labstatus.ReasonPubKeyCollision, msg)
			_ = r.Status().Update(ctx, lgc)
			r.Recorder.Event(lgc, corev1.EventTypeWarning, labstatus.ReasonPubKeyCollision, msg)
			ctrl.LoggerFrom(ctx).Error(
				fmt.Errorf("%s", msg),
				"refusing to allocate duplicate VPN client",
				"client",
				lgc.Name,
			)
			return ctrl.Result{}, nil
		}

		allocator := &poolpkg.ForwardAllocator{Client: r.Client, Prefix: names.PoolVPNClients, Namespace: lgc.Namespace, Size: 254, InUse: r.clientIndexesInUse(lgc.Namespace)}
		idx, err := allocator.AllocateIndex(ctx)
		if errors.Is(err, poolpkg.ErrFull) {
			return r.poolExhausted(ctx, lgc, err), nil
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("allocate VPN IP: %w", err)
		}
		assignedIP, err = r.vpnClientIP(idx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("compute client IP: %w", err)
		}
	}

	// Fetch parent LabGroup so we can populate serverPublicKey + endpoint in the
	// Secret. If the LabGroup is not yet Ready we still write what we have.
	serverPubKey, endpoint, lgErr := r.lookupParentVPN(ctx, lgc.Namespace)
	if lgErr != nil {
		// Don't fail hard — Secret is still useful with publicKey/assignedIP/privateKey.
		ctrl.LoggerFrom(ctx).V(1).Info("parent LabGroup not yet readable", "reason", lgErr.Error())
	}

	// Assemble the client config with a private-key PLACEHOLDER and store it in
	// status — never in a Secret, so the cluster never holds the private key.
	// Empty until the parent VPN endpoint/server key land (the requeue waits).
	allowed, err := r.reachableDestinations(ctx, lgc, assignedIP)
	if err != nil {
		return ctrl.Result{}, err
	}
	config, _ := renderWGConf(wgConfParams{
		AssignedIP:      assignedIP,
		ServerPublicKey: serverPubKey,
		Endpoint:        endpoint,
		AllowedIPs:      strings.Join(allowed, ", "),
	})

	updated := false
	if lgc.Status.AssignedIP != assignedIP {
		lgc.Status.AssignedIP = assignedIP
		updated = true
	}
	if lgc.Status.Config != config {
		lgc.Status.Config = config
		updated = true
	}
	if updated {
		if err := r.Status().Update(ctx, lgc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Requeue softly until LabGroup endpoint/server pubkey land so the wg.conf
	// gets refreshed without depending solely on the cross-resource watch.
	if serverPubKey == "" || endpoint == "" {
		if labstatus.IsReady(lgc.Status.Conditions) || len(lgc.Status.Conditions) == 0 || readyReason(lgc) == labstatus.ReasonVPNPoolExhausted {
			labstatus.SetReady(
				&lgc.Status.Conditions, lgc.Generation, false, labstatus.ReasonWaitingForVPNServer,
				"waiting for parent LabGroup VPN endpoint/public key",
			)
			_ = r.Status().Update(ctx, lgc)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !labstatus.IsReady(lgc.Status.Conditions) {
		labstatus.SetReady(
			&lgc.Status.Conditions,
			lgc.Generation,
			true,
			labstatus.ReasonReady,
			"client config provisioned",
		)
		_ = r.Status().Update(ctx, lgc)
		r.Recorder.Event(lgc, corev1.EventTypeNormal, labstatus.ReasonReady, "VPN client config ready")
	}
	return ctrl.Result{}, nil
}

// poolExhaustedRetry is how often a client that found the pool full tries again: an address comes free only some time after a client
// of the group is deleted (see poolpkg.ForwardAllocator), and nothing else signals it.
const poolExhaustedRetry = time.Minute

// poolExhausted reports a full VPN client pool on the client (Ready=False, reason VPNPoolExhausted, one Warning event when the state is
// entered) and asks for a slow retry instead of an error, which would requeue with backoff forever and show nothing on the object.
func (r *LabGroupClientReconciler) poolExhausted(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient, cause error) ctrl.Result {
	msg := fmt.Sprintf("no VPN address left for this client: %v; an address comes free a while after a client of the group is deleted", cause)
	if readyReason(lgc) != labstatus.ReasonVPNPoolExhausted || labstatus.IsReady(lgc.Status.Conditions) {
		labstatus.SetReady(&lgc.Status.Conditions, lgc.Generation, false, labstatus.ReasonVPNPoolExhausted, msg)
		if err := r.Status().Update(ctx, lgc); err != nil {
			ctrl.LoggerFrom(ctx).Info("status update of a client without a VPN address failed", "reason", err.Error())
			return ctrl.Result{RequeueAfter: poolExhaustedRetry}
		}
		r.Recorder.Event(lgc, corev1.EventTypeWarning, labstatus.ReasonVPNPoolExhausted, msg)
	}
	return ctrl.Result{RequeueAfter: poolExhaustedRetry}
}

// readyReason is the reason of the Ready condition ("" without one).
func readyReason(lgc *laboratoryv1alpha1.LabGroupClient) string {
	if c := meta.FindStatusCondition(lgc.Status.Conditions, labstatus.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func (r *LabGroupClientReconciler) reconcileDelete(
	ctx context.Context,
	lgc *laboratoryv1alpha1.LabGroupClient,
) (ctrl.Result, error) {
	// The tunnel address is not given back: the pool of a group only moves forward and goes with the group (see ForwardAllocator).

	// No client Secret to clean up — the config lives in status only.

	// The cybericebox.com/vpn finalizer is removed by the VPN binary after it
	// deletes the WireGuard peer. The VPN pod is a Deployment that always comes
	// back, so this is processed eventually; group teardown (where the pod would
	// not return) is handled by LabGroup.reconcileDelete draining clients first.
	controllerutil.RemoveFinalizer(lgc, names.FinalizerLabGroupClient)
	return ctrl.Result{}, r.Update(ctx, lgc)
}

// findDuplicatePubKey returns the name of another LabGroupClient in the same
// namespace that already uses pubKey, or "" if it's free. Checks both
// Spec.PublicKey (already-allocated peers) and skips the caller itself.
func (r *LabGroupClientReconciler) findDuplicatePubKey(
	ctx context.Context,
	pubKey string,
	self *laboratoryv1alpha1.LabGroupClient,
) (string, error) {
	if pubKey == "" {
		return "", nil
	}
	var list laboratoryv1alpha1.LabGroupClientList
	if err := r.List(ctx, &list, client.InNamespace(self.Namespace)); err != nil {
		return "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == self.Name {
			continue
		}
		if other.Spec.PublicKey == pubKey {
			return other.Name, nil
		}
	}
	return "", nil
}

// lookupParentVPN finds the LabGroup that owns the given namespace and returns
// its current VPN public key + endpoint. The namespace label
// "laboratory.cybericebox.com/group" points to the owning LabGroup name.
func (r *LabGroupClientReconciler) lookupParentVPN(ctx context.Context, ns string) (
	pubKey, endpoint string,
	err error,
) {
	var nsObj corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, &nsObj); err != nil {
		return "", "", err
	}
	owner := nsObj.Labels[names.LabelGroup]
	if owner == "" {
		return "", "", fmt.Errorf("namespace %q missing group label", ns)
	}
	var lg laboratoryv1alpha1.LabGroup
	if err := r.Get(ctx, types.NamespacedName{Name: owner}, &lg); err != nil {
		return "", "", err
	}
	return lg.Status.VPN.PublicKey, lg.Status.VPN.Endpoint, nil
}

type wgConfParams struct {
	AssignedIP      string
	ServerPublicKey string // empty until LabGroup is Ready
	Endpoint        string // empty until LabGroup is Ready
	AllowedIPs      string
}

// wgConfTemplate renders a WireGuard client config.
//
// Spec §4: no DNS= section (Apple-clients trigger a NetworkExtension bug).
// PersistentKeepalive 15 s — §4 recommends 10–25 s for roaming recovery.
// PrivateKey is the system-generated value when present; otherwise a literal
// placeholder is left so the user pastes their own.
var wgConfTemplate = template.Must(
	template.New("wg.conf").Parse(
		`[Interface]
PrivateKey = {{ .PrivateKey }}
Address = {{ .AssignedIP }}

[Peer]
PublicKey = {{ .ServerPublicKey }}
Endpoint = {{ .Endpoint }}
{{ if .AllowedIPs }}AllowedIPs = {{ .AllowedIPs }}
{{ end }}PersistentKeepalive = 15
`,
	),
)

// renderWGConf assembles the client config with a private-key PLACEHOLDER — the
// cluster never has the real private key; the caller substitutes it. Returns an
// error (empty config) until the parent VPN endpoint/server key are known.
func renderWGConf(p wgConfParams) (string, error) {
	if p.ServerPublicKey == "" || p.Endpoint == "" || p.AssignedIP == "" {
		return "", fmt.Errorf("incomplete params")
	}
	var buf bytes.Buffer
	if err := wgConfTemplate.Execute(
		&buf, struct {
			PrivateKey, AssignedIP, ServerPublicKey, Endpoint, AllowedIPs string
		}{names.WGPrivateKeyPlaceholder, p.AssignedIP, p.ServerPublicKey, p.Endpoint, p.AllowedIPs},
	); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// vpnClientIP computes the /32 address for pool slot idx within the first /24 of vpnBaseNet.
// idx=0 → .1 (first client; .0 is the subnet address, VPN gateway uses .1 conventionally).
func (r *LabGroupClientReconciler) vpnClientIP(idx uint) (string, error) {
	s, err := netutil.SubnetForIndex(r.LabBaseNetwork, labSubnetPrefixLen, 0)
	if err != nil {
		return "", fmt.Errorf("derive client subnet: %w", err)
	}
	ip, _, _ := net.ParseCIDR(s)
	base := ip.To4()
	return fmt.Sprintf("%d.%d.%d.%d/32", base[0], base[1], base[2], int(base[3])+int(idx)+1), nil
}

// ipToIndex extracts the pool index from a CIDR like "10.8.0.5/32".
// Inverse of the assignment formula "10.8.0.{idx+1}/32" → returns idx = last_octet - 1.
func ipToIndex(cidr string) (uint, error) {
	var a, b, c, d uint
	if n, err := fmt.Sscanf(cidr, "%d.%d.%d.%d/32", &a, &b, &c, &d); err != nil || n != 4 {
		return 0, fmt.Errorf("invalid CIDR %q", cidr)
	}
	if d == 0 {
		return 0, fmt.Errorf("invalid CIDR %q: last octet is 0", cidr)
	}
	return d - 1, nil
}

func (r *LabGroupClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Re-reconcile all LabGroupClient in a group's namespace when the parent
	// LabGroup status changes (so wg.conf picks up server pubkey / endpoint).
	groupMap := func(ctx context.Context, obj client.Object) []reconcile.Request {
		lg, ok := obj.(*laboratoryv1alpha1.LabGroup)
		if !ok {
			return nil
		}
		ns := lg.Status.Namespace
		if ns == "" {
			return nil
		}
		var lgcList laboratoryv1alpha1.LabGroupClientList
		if err := r.List(ctx, &lgcList, client.InNamespace(ns)); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(lgcList.Items))
		for _, lgc := range lgcList.Items {
			reqs = append(
				reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: lgc.Name, Namespace: lgc.Namespace},
				},
			)
		}
		return reqs
	}

	// The routes of a client follow the access policy and the gateways of the labs.
	inNamespace := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var lgcList laboratoryv1alpha1.LabGroupClientList
		if err := r.List(ctx, &lgcList, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(lgcList.Items))
		for _, lgc := range lgcList.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: lgc.Name, Namespace: lgc.Namespace}})
		}
		return reqs
	}
	gatewaysChanged := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		oldLab, ok1 := e.ObjectOld.(*laboratoryv1alpha1.Lab)
		newLab, ok2 := e.ObjectNew.(*laboratoryv1alpha1.Lab)
		return !ok1 || !ok2 || !reflect.DeepEqual(oldLab.Spec.Gateways, newLab.Spec.Gateways) || !reflect.DeepEqual(oldLab.Status.Gateways, newLab.Status.Gateways)
	}}
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroupClient{}).
		Watches(&laboratoryv1alpha1.LabGroup{}, handler.EnqueueRequestsFromMapFunc(groupMap)).
		// Counter refreshes patch only the status of the policy; a change of its spec (a new generation) is what moves the routes.
		Watches(&laboratoryv1alpha1.LabGroupAccessPolicy{}, handler.EnqueueRequestsFromMapFunc(inNamespace), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&laboratoryv1alpha1.Lab{}, handler.EnqueueRequestsFromMapFunc(inNamespace), builder.WithPredicates(gatewaysChanged)).
		Complete(reconcileutil.Quiet(r))
}

// clientIndexesInUse is the tunnel indexes the clients of a group's namespace hold, read straight from the API server (the insurance
// of the forward-only pool: see poolpkg.ForwardAllocator).
func (r *LabGroupClientReconciler) clientIndexesInUse(namespace string) func(context.Context) (map[uint]bool, error) {
	return func(ctx context.Context) (map[uint]bool, error) {
		var reader client.Reader = r.Client
		if r.Reader != nil {
			reader = r.Reader
		}
		var clients laboratoryv1alpha1.LabGroupClientList
		if err := reader.List(ctx, &clients, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		used := map[uint]bool{}
		for i := range clients.Items {
			if ip := clients.Items[i].Status.AssignedIP; ip != "" {
				if idx, err := ipToIndex(ip); err == nil {
					used[idx] = true
				}
			}
		}
		return used, nil
	}
}

// reachableDestinations is what the client may reach through the VPN: the subnets its allow rules name, over all the VPN gateways (connectors)
// of the group's labs. It is the AllowedIPs of the client's WireGuard config, so the client routes exactly that into the tunnel (the policy
// is enforced by the VPN pod in any case). A client no rule lets in gets an empty list. Both the policy and the lab gateways trigger this again.
func (r *LabGroupClientReconciler) reachableDestinations(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient, assignedIP string) ([]string, error) {
	var policy laboratoryv1alpha1.LabGroupAccessPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: lgc.Namespace, Name: names.LabGroupAccessPolicyName}, &policy); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	var labs laboratoryv1alpha1.LabList
	if err := r.List(ctx, &labs, client.InNamespace(lgc.Namespace)); err != nil {
		return nil, err
	}
	return vpn.ReachableDestinations(vpn.ClientAccessSnapshot{Name: lgc.Name, AssignedIP: assignedIP}, labs.Items, &policy), nil
}
