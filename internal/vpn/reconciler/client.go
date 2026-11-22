//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
	labstatus "github.com/cybericebox/laboratory/internal/status"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// LabGroupClientReconciler manages WireGuard peers for LabGroupClient resources.
type LabGroupClientReconciler struct {
	client.Client
	WG       *vpn.WGManager
	Cfg      *vpn.Config
	Recorder record.EventRecorder
}

func (r *LabGroupClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lgc laboratoryv1alpha1.LabGroupClient
	if err := r.Get(ctx, req.NamespacedName, &lgc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path — run before the finalizer add so cleanup happens even
	// while the operator's own finalizer is still present.
	if !lgc.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&lgc, names.FinalizerVPN) {
			if lgc.Spec.PublicKey != "" {
				_ = r.WG.RemovePeer(lgc.Spec.PublicKey)
			}
			controllerutil.RemoveFinalizer(&lgc, names.FinalizerVPN)
			return ctrl.Result{}, r.Update(ctx, &lgc)
		}
		return ctrl.Result{}, nil
	}

	// The operator allocates the IP and generates/persists the public key. Wait
	// for both before adding the peer — do NOT gate on the operator's finalizer,
	// which is a different finalizer than this reconciler's readiness signal.
	pubKey := lgc.Spec.PublicKey
	assignedIP := lgc.Status.AssignedIP
	if pubKey == "" || assignedIP == "" {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// First registration = finalizer not yet present. Capture before adding it.
	newPeer := !controllerutil.ContainsFinalizer(&lgc, names.FinalizerVPN)

	// Add own finalizer once the peer is about to be programmed, so teardown
	// removes the peer before the object is garbage-collected.
	if newPeer {
		controllerutil.AddFinalizer(&lgc, names.FinalizerVPN)
		if err := r.Update(ctx, &lgc); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &lgc); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if err := r.WG.AddPeer(pubKey, assignedIP); err != nil {
		r.Recorder.Eventf(
			&lgc, corev1.EventTypeWarning, labstatus.ReasonProgrammingFailed,
			"failed to register WireGuard peer: %v", err,
		)
		return ctrl.Result{}, fmt.Errorf("add WG peer %s: %w", lgc.Name, err)
	}
	if newPeer {
		r.Recorder.Eventf(
			&lgc, corev1.EventTypeNormal, labstatus.ReasonPeerRegistered,
			"WireGuard peer registered (allowedIP %s)", assignedIP,
		)
	}
	return ctrl.Result{}, nil
}

func (r *LabGroupClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroupClient{}, builder.WithPredicates(clientPeerChanges())).
		Complete(reconcileutil.Quiet(r))
}
