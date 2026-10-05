//go:build linux

package nodeagent

import (
	"context"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// DevicePortReconciler manages the ovs-cleanup finalizer on Device CRDs and
// stamps each local device with the node's Geneve VTEP address so that
// ConnectionReconciler on peer nodes can determine the tunnel destination.
// OVS port creation/deletion for container devices is handled by ConnectionReconciler.
// Switch/hub devices have no physical OVS resource; their finalizer is removed immediately on delete.
type DevicePortReconciler struct {
	client.Client
	NodeName    string
	NodeAddress string
}

func (r *DevicePortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var device laboratoryv1alpha1.Device
	if err := r.Get(ctx, req.NamespacedName, &device); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !device.DeletionTimestamp.IsZero() {
		// Wait for ConnectionReconciler to finish OVS cleanup for all connections
		// that include this device before removing the finalizer.
		var connList laboratoryv1alpha1.ConnectionList
		if err := r.List(
			ctx, &connList,
			client.InNamespace(device.Namespace),
			client.MatchingLabels{names.LabelLab: device.Spec.LabRef},
		); err != nil {
			return ctrl.Result{}, err
		}
		for _, conn := range connList.Items {
			if controllerutil.ContainsFinalizer(&conn, names.FinalizerOVSCleanup) {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
		controllerutil.RemoveFinalizer(&device, names.FinalizerOVSCleanup)
		return ctrl.Result{}, r.Update(ctx, &device)
	}

	// Add finalizer for switch/hub devices (no node affinity) or devices on this node.
	isSwitch := device.Spec.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch ||
		device.Spec.Type == laboratoryv1alpha1.DeviceTypeHub
	isLocal := device.Status.NodeName == r.NodeName

	if (isSwitch || isLocal) && !controllerutil.ContainsFinalizer(&device, names.FinalizerOVSCleanup) {
		controllerutil.AddFinalizer(&device, names.FinalizerOVSCleanup)
		return ctrl.Result{}, r.Update(ctx, &device)
	}

	// Stamp the Geneve VTEP address on the device status so that peer nodes'
	// ConnectionReconcilers can build the correct tunnel destination.
	if isLocal && r.NodeAddress != "" && device.Status.NodeAddress != r.NodeAddress {
		device.Status.NodeAddress = r.NodeAddress
		return ctrl.Result{}, r.Status().Update(ctx, &device)
	}

	return ctrl.Result{}, nil
}

func (r *DevicePortReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Device{}).
		Complete(reconcileutil.QuietIgnoreNotFound(r))
}
