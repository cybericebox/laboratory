package laboratory

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// BandwidthReconciler writes the bandwidth ceilings (names.AnnotationBandwidthLimits) onto the pods that apply them themselves: the
// gateways of the groups, the WireGuard demux and the L7 proxy. See package bandwidth for the numbers. It runs on events only: a
// node's label, a pod that appears, moves or goes (a reserve, a gateway, a proxy), a lab's gateway that is made or removed. All of
// them are one key, so a burst of events is one pass; a pass writes only the annotations that differ.
//
// The Geneve ceilings of a node are not written here: the node-agent works them out from the same numbers (see nodeagent.BandwidthReconciler),
// so the operator needs no right to write to nodes.
type BandwidthReconciler struct {
	Client client.Client
	Config bandwidth.Config
}

// Reconcile computes the plan and writes the annotations that are not as planned.
func (r *BandwidthReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods); err != nil {
		return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
	}
	var gateways laboratoryv1alpha1.LabGatewayList
	if err := r.Client.List(ctx, &gateways); err != nil {
		return ctrl.Result{}, fmt.Errorf("list lab gateways: %w", err)
	}
	labs := map[string]int{}
	for i := range gateways.Items {
		if gateways.Items[i].DeletionTimestamp.IsZero() {
			labs[gateways.Items[i].Namespace]++
		}
	}
	plan := bandwidthPlan(r.Config, nodes.Items, pods.Items, labs)

	var first error
	for i := range pods.Items {
		p := &pods.Items[i]
		want, ok := plan[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}]
		if !ok || p.Annotations[names.AnnotationBandwidthLimits] == want {
			continue
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{names.AnnotationBandwidthLimits: want}}})
		if err := r.Client.Patch(ctx, p, client.RawPatch(types.MergePatchType, patch)); client.IgnoreNotFound(err) != nil && first == nil {
			first = fmt.Errorf("annotate pod %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return ctrl.Result{}, first
}

// relevantBandwidthPod says a pod takes part: it carries a ceiling or declares a reserve.
func relevantBandwidthPod(o client.Object) bool {
	p, ok := o.(*corev1.Pod)
	if !ok {
		return false
	}
	return p.Annotations[names.AnnotationBandwidthReserve] != "" || bandwidthKindOf(p) != notBandwidthPod
}

// SetupWithManager reconciles one fixed key on the events that change the plan.
func (r *BandwidthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	key := reconcile.Request{NamespacedName: types.NamespacedName{Name: "bandwidth"}}
	toKey := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request { return []reconcile.Request{key} })
	pods := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return relevantBandwidthPod(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return relevantBandwidthPod(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !relevantBandwidthPod(e.ObjectNew) {
				return false
			}
			a, b := e.ObjectOld.(*corev1.Pod), e.ObjectNew.(*corev1.Pod)
			return a.Spec.NodeName != b.Spec.NodeName || a.Status.Phase != b.Status.Phase || a.DeletionTimestamp.IsZero() != b.DeletionTimestamp.IsZero() ||
				a.Annotations[names.AnnotationBandwidthReserve] != b.Annotations[names.AnnotationBandwidthReserve] ||
				a.Annotations[names.AnnotationBandwidthLimits] != b.Annotations[names.AnnotationBandwidthLimits]
		},
	}
	nodes := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetLabels()[names.LabelNodeBandwidth] != e.ObjectNew.GetLabels()[names.LabelNodeBandwidth]
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	gateways := predicate.Funcs{
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).Named("bandwidth").
		Watches(&corev1.Pod{}, toKey, builder.WithPredicates(pods)).
		Watches(&corev1.Node{}, toKey, builder.WithPredicates(nodes)).
		Watches(&laboratoryv1alpha1.LabGateway{}, toKey, builder.WithPredicates(gateways)).
		Complete(reconcileutil.Quiet(r))
}
