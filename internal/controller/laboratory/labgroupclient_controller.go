package laboratory

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

// LabGroupClientReconciler reconciles a LabGroupClient object.
type LabGroupClientReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// VPNBaseNetwork is the full VPN address space advertised to WireGuard clients (e.g. "10.8.0.0/10").
	VPNBaseNetwork string
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
	if !controllerutil.ContainsFinalizer(lgc, names.FinalizerLabGroupClient) {
		controllerutil.AddFinalizer(lgc, names.FinalizerLabGroupClient)
		if err := r.Update(ctx, lgc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Allocate IP + generate keypair on first pass only.
	pubKey := lgc.Spec.PublicKey
	var privKeyB64 []byte
	assignedIP := lgc.Status.AssignedIP

	if assignedIP == "" {
		if pubKey == "" {
			priv, pub, err := generateWireGuardKeypair()
			if err != nil {
				return ctrl.Result{}, err
			}
			pubKey = string(pub)
			privKeyB64 = priv
			// Persist generated pubkey into Spec so the VPN reconciler can register the peer.
			lgc.Spec.PublicKey = pubKey
			if err := r.Update(ctx, lgc); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Reject duplicate pubkey in the same group — peers are identified by
		// pubkey + assignedIP on the VPN server, so two LGCs with the same key
		// would race for the peer slot.
		if dup, dupErr := r.findDuplicatePubKey(ctx, pubKey, lgc); dupErr != nil {
			return ctrl.Result{}, dupErr
		} else if dup != "" {
			ctrl.LoggerFrom(ctx).Error(fmt.Errorf("pubkey collision with %q", dup),
				"refusing to allocate duplicate VPN client", "client", lgc.Name)
			return ctrl.Result{}, nil
		}

		allocator := poolpkg.NewAllocator(r.Client, names.PoolVPNClients, lgc.Namespace, 254)
		idx, err := allocator.AllocateIndex(ctx)
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

	secretName := names.SecretClientPrefix + lgc.Name
	if err := r.ensureClientSecret(ctx, lgc, secretName, secretParams{
		PublicKey:       pubKey,
		PrivateKey:      string(privKeyB64),
		AssignedIP:      assignedIP,
		ServerPublicKey: serverPubKey,
		Endpoint:        endpoint,
		AllowedIPs:      r.VPNBaseNetwork,
	}); err != nil {
		return ctrl.Result{}, err
	}

	updated := false
	if lgc.Status.AssignedIP != assignedIP {
		lgc.Status.AssignedIP = assignedIP
		updated = true
	}
	if lgc.Status.SecretRef != secretName {
		lgc.Status.SecretRef = secretName
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
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *LabGroupClientReconciler) reconcileDelete(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient) (ctrl.Result, error) {
	if lgc.Status.AssignedIP != "" {
		allocator := poolpkg.NewAllocator(r.Client, names.PoolVPNClients, lgc.Namespace, 254)
		idx, err := ipToIndex(lgc.Status.AssignedIP)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := allocator.ReleaseIndex(ctx, idx); err != nil {
			return ctrl.Result{}, err
		}
	}

	secretName := names.SecretClientPrefix + lgc.Name
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: lgc.Namespace}, &s); err == nil {
		if err = r.Delete(ctx, &s); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(lgc, names.FinalizerLabGroupClient)
	return ctrl.Result{}, r.Update(ctx, lgc)
}

// findDuplicatePubKey returns the name of another LabGroupClient in the same
// namespace that already uses pubKey, or "" if it's free. Checks both
// Spec.PublicKey (already-allocated peers) and skips the caller itself.
func (r *LabGroupClientReconciler) findDuplicatePubKey(ctx context.Context, pubKey string, self *laboratoryv1alpha1.LabGroupClient) (string, error) {
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
func (r *LabGroupClientReconciler) lookupParentVPN(ctx context.Context, ns string) (pubKey, endpoint string, err error) {
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

type secretParams struct {
	PublicKey       string
	PrivateKey      string // may be empty (user-supplied keypair)
	AssignedIP      string
	ServerPublicKey string // empty until LabGroup is Ready
	Endpoint        string // empty until LabGroup is Ready
	AllowedIPs      string
}

func (r *LabGroupClientReconciler) ensureClientSecret(ctx context.Context, lgc *laboratoryv1alpha1.LabGroupClient, name string, p secretParams) error {
	desired := map[string][]byte{
		"publicKey":  []byte(p.PublicKey),
		"assignedIP": []byte(p.AssignedIP),
		"allowedIPs": []byte(p.AllowedIPs),
	}
	if p.PrivateKey != "" {
		desired["privateKey"] = []byte(p.PrivateKey)
	}
	if p.ServerPublicKey != "" {
		desired["serverPublicKey"] = []byte(p.ServerPublicKey)
	}
	if p.Endpoint != "" {
		desired["endpoint"] = []byte(p.Endpoint)
	}
	if conf, err := renderWGConf(p); err == nil {
		desired["wg.conf"] = []byte(conf)
	}

	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lgc.Namespace}, &existing)
	if errors.IsNotFound(err) {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lgc.Namespace},
			Data:       desired,
		}
		return r.Create(ctx, s)
	}
	if err != nil {
		return err
	}

	// Preserve existing privateKey across reconciles — it's generated once and never regenerated.
	if _, ok := desired["privateKey"]; !ok {
		if pk, ok := existing.Data["privateKey"]; ok {
			desired["privateKey"] = pk
			// Re-render wg.conf with the preserved privateKey if endpoint is now available.
			if p.Endpoint != "" && p.ServerPublicKey != "" {
				p.PrivateKey = string(pk)
				if conf, cerr := renderWGConf(p); cerr == nil {
					desired["wg.conf"] = []byte(conf)
				}
			}
		}
	}

	if dataEqual(existing.Data, desired) {
		return nil
	}
	existing.Data = desired
	return r.Update(ctx, &existing)
}

func dataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(b[k], v) {
			return false
		}
	}
	return true
}

// wgConfTemplate renders a WireGuard client config.
//
// Spec §4: no DNS= section (Apple-clients trigger a NetworkExtension bug).
// PersistentKeepalive 15 s — §4 recommends 10–25 s for roaming recovery.
// PrivateKey is the system-generated value when present; otherwise a literal
// placeholder is left so the user pastes their own.
var wgConfTemplate = template.Must(template.New("wg.conf").Parse(`[Interface]
PrivateKey = {{ .PrivateKey }}
Address = {{ .AssignedIP }}

[Peer]
PublicKey = {{ .ServerPublicKey }}
Endpoint = {{ .Endpoint }}
AllowedIPs = {{ .AllowedIPs }}
PersistentKeepalive = 15
`))

func renderWGConf(p secretParams) (string, error) {
	if p.ServerPublicKey == "" || p.Endpoint == "" || p.AssignedIP == "" || p.AllowedIPs == "" {
		return "", fmt.Errorf("incomplete params")
	}
	priv := p.PrivateKey
	if priv == "" {
		priv = "<YOUR_PRIVATE_KEY>"
	}
	var buf bytes.Buffer
	if err := wgConfTemplate.Execute(&buf, struct {
		PrivateKey, AssignedIP, ServerPublicKey, Endpoint, AllowedIPs string
	}{priv, p.AssignedIP, p.ServerPublicKey, p.Endpoint, p.AllowedIPs}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// vpnClientIP computes the /32 address for pool slot idx within the first /24 of vpnBaseNet.
// idx=0 → .1 (first client; .0 is the subnet address, VPN gateway uses .1 conventionally).
func (r *LabGroupClientReconciler) vpnClientIP(idx uint) (string, error) {
	s, err := netutil.SubnetForIndex(r.VPNBaseNetwork, labSubnetPrefixLen, 0)
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
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: lgc.Name, Namespace: lgc.Namespace},
			})
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroupClient{}).
		Watches(&laboratoryv1alpha1.LabGroup{}, handler.EnqueueRequestsFromMapFunc(groupMap)).
		Complete(r)
}
