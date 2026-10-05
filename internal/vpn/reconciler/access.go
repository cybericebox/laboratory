//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// AccessReconciler materializes all LabGroupClient allow-lists into the VPN
// server's packet filter. It is intentionally a full snapshot reconcile: one
// client update, lab readiness transition, or deletion recomputes default-deny
// rules for the whole group namespace.
type AccessReconciler struct {
	client.Client
	IPT *vpn.IPTablesManager
	// Conntrack removes the open connections a rule change revokes: FORWARD accepts
	// established connections before the access chain, so replacing the chain alone
	// would leave an open SSH session or download running. Nil: not removed (tests).
	Conntrack *vpn.ConntrackRevoker
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
	policyFound := err == nil
	rules := vpn.BuildAccessRules(clientSnapshots, labsByName, policyRules(policy))
	if err := r.IPT.ReplaceAccessRules(rules); err != nil {
		if policyFound {
			_ = r.writePolicyStatus(ctx, policy, rules, nil, "Failed", err.Error())
		}
		return ctrl.Result{}, err
	}
	if r.Conntrack != nil {
		labCIDRs := make([]string, 0, len(labsByName))
		for _, l := range labsByName {
			if l.VPNCIDR != "" {
				labCIDRs = append(labCIDRs, l.VPNCIDR)
			}
		}
		n, err := r.Conntrack.Revoke(labCIDRs, rules)
		if n > 0 {
			ctrl.LoggerFrom(ctx).Info("closed connections the access rules no longer allow", "connections", n)
		}
		if err != nil {
			// The rules are in place; the open connections are not all gone. Run again.
			return ctrl.Result{}, err
		}
	}
	if policyFound {
		counters, countersErr := r.IPT.AccessCounters()
		if countersErr != nil {
			return ctrl.Result{}, r.writePolicyStatus(ctx, policy, rules, nil, "Failed", countersErr.Error())
		}
		if statusErr := r.writePolicyStatus(ctx, policy, rules, counters, "Applied", ""); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, nil
}

func policyRules(policy *laboratoryv1alpha1.LabGroupAccessPolicy) []vpn.AccessPolicyRule {
	rules := make([]vpn.AccessPolicyRule, 0, len(policy.Spec.Rules))
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
		rules = append(rules, vpn.AccessPolicyRule{Action: action, ClientNames: rule.ClientNames, LabNames: rule.LabNames})
	}
	return rules
}

func (r *AccessReconciler) writePolicyStatus(ctx context.Context, policy *laboratoryv1alpha1.LabGroupAccessPolicy, rules []vpn.AccessRule, counters map[string]vpn.TrafficCounter, state, lastError string) error {
	previous := make(map[string]vpn.TrafficCounter, len(policy.Status.Rules))
	for _, status := range policy.Status.Rules {
		previous[vpn.AccessRule{ClientName: status.ClientName, LabName: status.LabName, Action: vpn.AccessAction(status.Action)}.Identifier()] = vpn.TrafficCounter{Packets: status.Packets, Bytes: status.Bytes}
	}
	statistics := vpn.ProjectAccessStatistics(rules, counters, previous)
	base := policy.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.State = state
	policy.Status.LastError = lastError
	policy.Status.AppliedAt = metav1.Now()
	policy.Status.Rules = make([]laboratoryv1alpha1.LabGroupAccessPolicyRuleStatus, 0, len(statistics))
	for _, statistic := range statistics {
		policy.Status.Rules = append(policy.Status.Rules, laboratoryv1alpha1.LabGroupAccessPolicyRuleStatus{
			ClientName:   statistic.ClientName,
			LabName:      statistic.LabName,
			Action:       laboratoryv1alpha1.LabGroupAccessAction(statistic.Action),
			Packets:      statistic.Packets,
			Bytes:        statistic.Bytes,
			CounterReset: statistic.CounterReset,
		})
	}
	if err := r.Status().Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch access policy status: %w", err)
	}
	return nil
}

// RunAccessStats refreshes firewall counters without changing the policy. The
// history is retained on the policy CR so the agent can relay it to the
// platform while an event is active.
func RunAccessStats(ctx context.Context, c client.Client, ipt *vpn.IPTablesManager, cfg *vpn.Config) {
	ticker := time.NewTicker(cfg.StatsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			policy := &laboratoryv1alpha1.LabGroupAccessPolicy{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: names.LabGroupAccessPolicyName}, policy); err != nil {
				continue
			}
			var clients laboratoryv1alpha1.LabGroupClientList
			var labs laboratoryv1alpha1.LabList
			if c.List(ctx, &clients, client.InNamespace(cfg.Namespace)) != nil || c.List(ctx, &labs, client.InNamespace(cfg.Namespace)) != nil {
				continue
			}
			labSnapshots := make(map[string]vpn.LabAccessSnapshot, len(labs.Items))
			for i := range labs.Items {
				labSnapshots[labs.Items[i].Name] = vpn.LabAccessSnapshot{VPNCIDR: labs.Items[i].Status.VPN.CIDR, Ready: labs.Items[i].Status.Phase == laboratoryv1alpha1.PhaseReady && labs.Items[i].Status.VPN.Ready}
			}
			clientSnapshots := make([]vpn.ClientAccessSnapshot, 0, len(clients.Items))
			for i := range clients.Items {
				clientSnapshots = append(clientSnapshots, vpn.ClientAccessSnapshot{Name: clients.Items[i].Name, AssignedIP: clients.Items[i].Status.AssignedIP})
			}
			counters, err := ipt.AccessCounters()
			if err != nil {
				continue
			}
			reconciler := &AccessReconciler{Client: c, IPT: ipt}
			_ = reconciler.writePolicyStatus(ctx, policy, vpn.BuildAccessRules(clientSnapshots, labSnapshots, policyRules(policy)), counters, "Applied", "")
		}
	}
}

func (r *AccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Requests carry only the namespace; Reconcile always reads the complete
	// snapshot, so a synthetic name is enough for Lab-driven changes.
	allInNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: object.GetNamespace(), Name: "access-policy"}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		// The peer reconciler also watches LabGroupClient; without its own name
		// both derive "labgroupclient" and the manager refuses the second one.
		Named("vpn-access").
		For(&laboratoryv1alpha1.LabGroupClient{}).
		Watches(&laboratoryv1alpha1.Lab{}, allInNamespace).
		// Counter refreshes patch only status. Do not turn those patches into
		// another policy reconcile; specification changes still enqueue one.
		Watches(&laboratoryv1alpha1.LabGroupAccessPolicy{}, allInNamespace, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(reconcileutil.Quiet(r))
}
