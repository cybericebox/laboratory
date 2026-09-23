//go:build linux

package reconciler

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// AccessReconciler materializes all LabGroupClient allow-lists into the VPN
// server's packet filter. It is intentionally a full snapshot reconcile: one
// client update, lab readiness transition, or deletion recomputes default-deny
// rules for the whole group namespace.
type AccessReconciler struct {
	client.Client
	IPT *vpn.IPTablesManager
}

func (r *AccessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var clients laboratoryv1alpha1.LabGroupClientList
	if err := r.List(ctx, &clients, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list lab group clients: %w", err)
	}
	var labs laboratoryv1alpha1.LabList
	if err := r.List(ctx, &labs, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list labs: %w", err)
	}

	labsByName := make(map[string]vpn.LabAccessSnapshot, len(labs.Items))
	for i := range labs.Items {
		lab := &labs.Items[i]
		labsByName[lab.Name] = vpn.LabAccessSnapshot{
			VPNCIDR: lab.Status.VPN.CIDR,
			Ready:   lab.Status.Phase == laboratoryv1alpha1.PhaseReady && lab.Status.VPN.Ready,
		}
	}
	clientSnapshots := make([]vpn.ClientAccessSnapshot, 0, len(clients.Items))
	for i := range clients.Items {
		client := &clients.Items[i]
		clientSnapshots = append(clientSnapshots, vpn.ClientAccessSnapshot{
			Name:       client.Name,
			AssignedIP: client.Status.AssignedIP,
		})
	}
	policy := &laboratoryv1alpha1.LabGroupAccessPolicy{}
	err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: names.LabGroupAccessPolicyName}, policy)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get group access policy: %w", err)
	}
	policyRules := make([]vpn.AccessPolicyRule, 0, len(policy.Spec.Rules))
	if err == nil {
		for _, rule := range policy.Spec.Rules {
			var action vpn.AccessAction
			switch rule.Action {
			case laboratoryv1alpha1.LabGroupAccessAllow:
				action = vpn.AccessAllow
			case laboratoryv1alpha1.LabGroupAccessDeny:
				action = vpn.AccessDeny
			default:
				continue
			}
			policyRules = append(policyRules, vpn.AccessPolicyRule{Action: action, ClientNames: rule.ClientNames, LabNames: rule.LabNames})
		}
	}
	if err := r.IPT.ReplaceAccessRules(vpn.BuildAccessRules(clientSnapshots, labsByName, policyRules)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *AccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Requests carry only the namespace; Reconcile always reads the complete
	// snapshot, so a synthetic name is enough for Lab-driven changes.
	allInNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: object.GetNamespace(), Name: "access-policy"}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroupClient{}).
		Watches(&laboratoryv1alpha1.Lab{}, allInNamespace).
		Watches(&laboratoryv1alpha1.LabGroupAccessPolicy{}, allInNamespace).
		Complete(r)
}
