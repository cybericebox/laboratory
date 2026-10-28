package nodeagent

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"
)

// GeneveLimiter is what the reconciler drives: the flow manager.
type GeneveLimiter interface {
	SetGeneveLimits(bandwidth.Limits) error
}

// BandwidthReconciler gives the flow manager the Geneve ceilings of this node. It watches nothing but the node itself and the
// pods that declare a bandwidth reserve on it, so it runs when the label or a reserve changes, never on a timer; the flow manager
// touches an OVS meter only when a rate changes.
//
// The numbers come from the same package the operator uses for the pods (shares, caps, the node's label and the reserves of the
// pods on it), so the operator's ceilings for the entry and the internet and the node-agent's for Geneve add up to the node's
// bandwidth. The node-agent works them out itself (it already reads its node and the pods on it), so the operator needs no
// permission to write to nodes.
type BandwidthReconciler struct {
	Client   client.Client
	NodeName string
	Config   bandwidth.Config
	Flows    GeneveLimiter
}

// Reconcile computes the limits of the node and applies them.
func (r *BandwidthReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": r.NodeName}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list the pods of the node: %w", err)
	}
	limits := r.Config.NodeGeneveLimits(r.Config.ForNode(&node, pods.Items))
	if err := r.Flows.SetGeneveLimits(limits); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply the Geneve limits: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager reconciles on this node's own events and on the pods of this node that carry a reserve.
func (r *BandwidthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	own := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == r.NodeName })
	reserve := func(o client.Object) bool {
		p, ok := o.(*corev1.Pod)
		return ok && p.Spec.NodeName == r.NodeName && p.Annotations[names.AnnotationBandwidthReserve] != ""
	}
	toNode := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if !reserve(o) {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: r.NodeName}}}
	})
	return ctrl.NewControllerManagedBy(mgr).Named("bandwidth").
		For(&corev1.Node{}, builder.WithPredicates(own)).
		Watches(&corev1.Pod{}, toNode, builder.WithPredicates(predicate.NewPredicateFuncs(reserve))).
		Complete(r)
}
