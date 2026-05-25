//go:build linux

package main

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// DevicePortReconciler manages the ovs-cleanup finalizer on Device CRDs.
// OVS port creation/deletion for container/vm devices is handled by ConnectionReconciler.
// Switch/hub devices have no physical OVS resource; their finalizer is removed immediately on delete.
type DevicePortReconciler struct {
	client.Client
	NodeName string
}

func (r *DevicePortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var device laboratoryv1alpha1.Device
	if err := r.Get(ctx, req.NamespacedName, &device); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !device.DeletionTimestamp.IsZero() {
		controllerutil.RemoveFinalizer(&device, laboratoryv1alpha1.FinalizerOVSCleanup)
		return ctrl.Result{}, r.Update(ctx, &device)
	}

	// Add finalizer for switch/hub devices (no node affinity) or devices on this node.
	isSwitch := device.Spec.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch ||
		device.Spec.Type == laboratoryv1alpha1.DeviceTypeHub
	isLocal := device.Status.NodeName == r.NodeName

	if (isSwitch || isLocal) && !controllerutil.ContainsFinalizer(&device, laboratoryv1alpha1.FinalizerOVSCleanup) {
		controllerutil.AddFinalizer(&device, laboratoryv1alpha1.FinalizerOVSCleanup)
		return ctrl.Result{}, r.Update(ctx, &device)
	}

	return ctrl.Result{}, nil
}

func (r *DevicePortReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Device{}).
		Complete(r)
}
