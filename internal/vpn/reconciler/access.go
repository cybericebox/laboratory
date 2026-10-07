//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"slices"

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
// server's packet filter. A relevant input change recomputes the desired group
// snapshot; the kernel is changed only when that snapshot differs.
type accessApplier interface {
	ReplaceAccessRules([]vpn.AccessRule) error
	AccessCounters() (map[string]vpn.TrafficCounter, error)
}

type accessRevoker interface {
	Revoke([]string, []vpn.AccessRule, ...[]string) (int, error)
}

type AccessReconciler struct {
	RequireInitialRetirement bool
	InitialBindings          map[string]bool
	client.Client
	IPT      accessApplier
	Counters func() map[string]vpn.TrafficCounter
	// Conntrack retires flows after a permission or identity change. The gate
	// checks both directions before established forwarding. Nil is used in tests.
	Conntrack accessRevoker
	// Controller-runtime serializes Reconcile calls for this controller.
	applied           bool
	lastNamespace     string
	lastRules         []vpn.AccessRule
	revokePending     bool
	revokeLabs        []string
	revokeClients     []string
	revokeReissued    map[string]bool
	activationPending bool
	initialChecked    bool
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

	var legs laboratoryv1alpha1.LabVPNList
	if err := r.List(ctx, &legs, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list VPN lab legs: %w", err)
	}
	labsByName, bindingErr := buildLabAccessSnapshots(labs.Items, legs.Items)
	if bindingErr != nil {
		return ctrl.Result{}, bindingErr
	}
	clientSnapshots := make([]vpn.ClientAccessSnapshot, 0, len(clients.Items))
	for i := range clients.Items {
		client := &clients.Items[i]
		if !client.DeletionTimestamp.IsZero() {
			continue
		}
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
	previousRules := r.lastRules
	changed := !r.applied || r.lastNamespace != req.Namespace || !slices.Equal(r.lastRules, rules)
	if changed {
		if r.revokeReissued == nil {
			r.revokeReissued = map[string]bool{}
		}
		if r.RequireInitialRetirement && !r.initialChecked {
			for _, rule := range rules {
				if rule.Action == vpn.AccessAllow && !r.InitialBindings[rule.BindingID()] {
					r.revokeReissued[rule.Identifier()] = true
				}
			}
		}
		for _, old := range previousRules {
			if old.Action != vpn.AccessAllow {
				continue
			}
			for _, next := range rules {
				if next.Action != vpn.AccessAllow {
					continue
				}
				if old.SourceCIDR != "" && old.SourceCIDR == next.SourceCIDR && old.ClientName != next.ClientName || old.LabInterface != "" && old.LabInterface == next.LabInterface && old.LabName != next.LabName {
					r.revokeReissued[next.Identifier()] = true
				}
			}
		}
		for _, old := range previousRules {
			r.revokeLabs = append(r.revokeLabs, old.DestinationCIDR)
			r.revokeClients = append(r.revokeClients, old.SourceCIDR)
		}
		for _, l := range labsByName {
			if l.VPNCIDR != "" {
				r.revokeLabs = append(r.revokeLabs, l.VPNCIDR)
			}
		}
		for _, c := range clientSnapshots {
			r.revokeClients = append(r.revokeClients, c.AssignedIP)
		}
		slices.Sort(r.revokeLabs)
		slices.Sort(r.revokeClients)
		r.revokeLabs = slices.Compact(r.revokeLabs)
		r.revokeClients = slices.Compact(r.revokeClients)
		// A legacy non-atomic replacement can fail after touching the chain:
		// invalidate the old snapshot before trying, so reverting still repairs it.
		r.applied = false
		safeRules := slices.Clone(rules)
		for i := range safeRules {
			if r.revokeReissued[safeRules[i].Identifier()] {
				safeRules[i].Action = vpn.AccessDeny
				r.activationPending = true
			}
		}
		if err := r.IPT.ReplaceAccessRules(safeRules); err != nil {
			if policyFound {
				_ = r.writePolicyStatus(ctx, policy, rules, nil, "Failed", err.Error())
			}
			return ctrl.Result{}, err
		}
		r.lastRules = slices.Clone(rules)
		r.lastNamespace = req.Namespace
		r.applied = true
		r.revokePending = true
	}
	if r.Conntrack != nil && r.revokePending {
		revocationRules := slices.Clone(rules)
		for i := range revocationRules {
			if r.revokeReissued[revocationRules[i].Identifier()] {
				revocationRules[i].Action = vpn.AccessDeny
			}
		}
		n, err := r.Conntrack.Revoke(r.revokeLabs, revocationRules, r.revokeClients)
		if n > 0 {
			ctrl.LoggerFrom(ctx).Info("closed connections the access rules no longer allow", "connections", n)
		}
		if err != nil {
			// The rules are in place; the open connections are not all gone. Run again.
			if policyFound {
				counters, _ := r.accessCounters()
				_ = r.writePolicyStatus(ctx, policy, rules, counters, "Failed", err.Error())
			}
			return ctrl.Result{}, err
		}
	}
	if r.activationPending {
		if r.Conntrack == nil {
			return ctrl.Result{}, fmt.Errorf("identity activation requires conntrack retirement")
		}
		if err := r.IPT.ReplaceAccessRules(rules); err != nil {
			r.applied = false
			return ctrl.Result{}, err
		}
		r.activationPending = false
	}
	r.initialChecked = true
	r.revokePending = false
	r.revokeLabs = nil
	r.revokeClients = nil
	r.revokeReissued = nil
	if policyFound {
		counters, countersErr := r.accessCounters()
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

func (r *AccessReconciler) accessCounters() (map[string]vpn.TrafficCounter, error) {
	if r.Counters != nil {
		return r.Counters(), nil
	}
	return r.IPT.AccessCounters()
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
		Watches(&laboratoryv1alpha1.LabGroupClient{}, allInNamespace, builder.WithPredicates(clientAccessChanges())).
		Watches(&laboratoryv1alpha1.Lab{}, allInNamespace, builder.WithPredicates(labAccessChanges())).
		Watches(&laboratoryv1alpha1.LabVPN{}, allInNamespace, builder.WithPredicates(labVPNAccessChanges())).
		// Counter refreshes patch only status. Do not turn those patches into
		// another policy reconcile; specification changes still enqueue one.
		Watches(&laboratoryv1alpha1.LabGroupAccessPolicy{}, allInNamespace, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(reconcileutil.Quiet(r))
}
