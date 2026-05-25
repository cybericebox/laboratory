//go:build linux

package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/ovsnames"
)

// LabIfaceReconciler creates the lab-<name> OVS internal port for each Lab's VPN and gateway pods.
type LabIfaceReconciler struct {
	client.Client
	NodeName string
	OVS      *OVSManager
	Server   *NodeAgentServer
	ProcRoot string
}

func (r *LabIfaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lab laboratoryv1alpha1.Lab
	if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lab.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lab)
	}

	needsRequeue := false

	if lab.Spec.VPN.Enabled && lab.Status.VPN.CIDR != "" {
		result, err := r.ensureLabIface(ctx, lab.Namespace, "vpn", ovsnames.LabIfaceName(lab.Name))
		if err != nil {
			return ctrl.Result{}, err
		}
		if result.RequeueAfter > 0 {
			needsRequeue = true
		}
	}

	if lab.Spec.Internet.Enabled && lab.Status.Internet.CIDR != "" {
		result, err := r.ensureLabIface(ctx, lab.Namespace, "gateway", ovsnames.LabGWIfaceName(lab.Name))
		if err != nil {
			return ctrl.Result{}, err
		}
		if result.RequeueAfter > 0 {
			needsRequeue = true
		}
	}

	if needsRequeue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *LabIfaceReconciler) ensureLabIface(ctx context.Context, namespace, appLabel, ifaceName string) (ctrl.Result, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace),
		client.MatchingLabels{"app": appLabel}); err != nil {
		return ctrl.Result{}, err
	}

	var localPod *corev1.Pod
	for i := range podList.Items {
		if podList.Items[i].Spec.NodeName == r.NodeName && podList.Items[i].Status.Phase == corev1.PodRunning {
			localPod = &podList.Items[i]
			break
		}
	}
	if localPod == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	netnsPath, ok := r.Server.GetPodNetNS(string(localPod.UID))
	if !ok {
		var err error
		netnsPath, err = FindPodNetNS(r.ProcRoot, string(localPod.UID))
		if err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if err := r.OVS.AddInternalPort(ifaceName); err != nil {
		return ctrl.Result{}, fmt.Errorf("add OVS internal port %q: %w", ifaceName, err)
	}

	// Move to pod netns; ignore ENODEV if already moved on a previous reconcile.
	_ = MoveToNetNS(ifaceName, netnsPath)

	return ctrl.Result{}, nil
}

func (r *LabIfaceReconciler) reconcileDelete(_ context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	_ = r.OVS.DelPort(ovsnames.LabIfaceName(lab.Name))
	_ = r.OVS.DelPort(ovsnames.LabGWIfaceName(lab.Name))
	return ctrl.Result{}, nil
}

func (r *LabIfaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapFn := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pod := obj.(*corev1.Pod)
		app, ok := pod.Labels["app"]
		if !ok || (app != "vpn" && app != "gateway") {
			return nil
		}
		var labList laboratoryv1alpha1.LabList
		if err := r.List(ctx, &labList, client.InNamespace(pod.Namespace)); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(labList.Items))
		for _, lab := range labList.Items {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: lab.Name, Namespace: lab.Namespace},
			})
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Lab{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapFn)).
		Complete(r)
}
