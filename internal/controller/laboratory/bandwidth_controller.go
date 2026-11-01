package laboratory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// BandwidthConfigMap is the ConfigMap of the per-node limits: one in the namespace of the WireGuard demux, one in that of the L7 proxy
// and one in that of the node-agent, a key per node (the value is what the pods on that node apply: the entry limits for the demux and the
// L7 proxy, the Geneve ceilings for the node-agent). A pod reads the key of its own node from the mounted file.
const BandwidthConfigMap = "laboratory-bandwidth"

// BandwidthReconciler writes the bandwidth ceilings the pods apply themselves. See package bandwidth for the numbers. The gateways
// of the groups get theirs as an annotation (names.AnnotationBandwidthLimits) of the pod, which the pod reads from a downward API file;
// the WireGuard demux, the L7 proxy and the node-agent get the limits of every node in a ConfigMap of their namespace (the demux may
// run on the host network and the node-agent is privileged, both are outside what the operator may patch). It runs on events only: a node's label, a pod that
// appears, moves or goes (a reserve, a gateway), a lab's gateway that is made or removed. All of them are one key, so a burst of
// events is one pass; a pass writes only what differs.
//
// The Geneve ceilings of a node are in the same ConfigMap (see nodeagent.BandwidthWatcher): the operator already knows the node's label, the
// reserves of the platform pods and the labs, so the node-agent computes nothing, watches no pod, and the operator needs no right to write to nodes.
type BandwidthReconciler struct {
	Client client.Client
	// Reader reads the ConfigMaps without a cache: the operator reads no ConfigMap cluster-wide.
	Reader client.Reader
	Config bandwidth.Config
	// EntryNamespaces are the namespaces that get the ConfigMap: those of the WireGuard demux, the L7 proxy and the node-agent.
	EntryNamespaces []string
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
	remember := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		want, ok := plan.gateways[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}]
		if !ok || p.Annotations[names.AnnotationBandwidthLimits] == want {
			continue
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{names.AnnotationBandwidthLimits: want}}})
		if err := r.Client.Patch(ctx, p, client.RawPatch(types.MergePatchType, patch)); client.IgnoreNotFound(err) != nil {
			remember(fmt.Errorf("annotate pod %s/%s: %w", p.Namespace, p.Name, err))
		}
	}
	for _, ns := range r.EntryNamespaces {
		remember(r.writeNodeLimits(ctx, ns, plan.nodes))
	}
	return ctrl.Result{}, first
}

// writeNodeLimits makes the ConfigMap of a namespace hold the limits of every node.
func (r *BandwidthReconciler) writeNodeLimits(ctx context.Context, ns string, nodes map[string]string) error {
	var cm corev1.ConfigMap
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: BandwidthConfigMap}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: BandwidthConfigMap}, Data: nodes}
		if err := r.Client.Create(ctx, &cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the bandwidth ConfigMap in %s: %w", ns, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read the bandwidth ConfigMap in %s: %w", ns, err)
	}
	if maps.Equal(cm.Data, nodes) {
		return nil
	}
	cm.Data = nodes
	if err := r.Client.Update(ctx, &cm); err != nil {
		return fmt.Errorf("update the bandwidth ConfigMap in %s: %w", ns, err)
	}
	return nil
}

// relevantBandwidthPod says a pod takes part: it carries a ceiling or declares a reserve.
func relevantBandwidthPod(o client.Object) bool {
	p, ok := o.(*corev1.Pod)
	if !ok {
		return false
	}
	return p.Annotations[names.AnnotationBandwidthReserve] != "" || isBandwidthGateway(p)
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
