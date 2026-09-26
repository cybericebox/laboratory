//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labdhcp"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

// LabVPNReconciler manages per-lab WireGuard routing and optional DHCP.
type LabVPNReconciler struct {
	client.Client
	WG       *vpn.WGManager
	DHCP     *dhcp.Manager
	Cfg      *vpn.Config
	Recorder record.EventRecorder
}

func (r *LabVPNReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("vpn").WithValues("labvpn", req.NamespacedName)

	var labvpn laboratoryv1alpha1.LabVPN
	if err := r.Get(ctx, req.NamespacedName, &labvpn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path — checked before the guard so cleanup runs even after
	// main controller removes FinalizerController to unblock this reconciler.
	if !labvpn.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&labvpn, names.FinalizerVPN) {
			return r.reconcileDelete(ctx, &labvpn)
		}
		return ctrl.Result{}, nil
	}

	// Main controller must have processed this first.
	if !controllerutil.ContainsFinalizer(&labvpn, names.FinalizerController) {
		return ctrl.Result{}, nil
	}

	// Add own finalizer on first observation.
	if !controllerutil.ContainsFinalizer(&labvpn, names.FinalizerVPN) {
		controllerutil.AddFinalizer(&labvpn, names.FinalizerVPN)
		return ctrl.Result{}, r.Update(ctx, &labvpn)
	}

	// Wait for lab{N} interface (created by node-agent via OVS).
	ifaceName := names.LabIfaceNameByIndex(labvpn.Spec.NetworkIndex)
	if _, err := netlink.LinkByName(ifaceName); err != nil {
		if labvpn.Status.Phase != laboratoryv1alpha1.LabVPNPhaseWaitingForInterface {
			r.Recorder.Eventf(
				&labvpn, corev1.EventTypeWarning, labstatus.ReasonWaitingForInterface,
				"waiting for OVS interface %q (node-agent has not attached it yet)", ifaceName,
			)
			if patchErr := r.patchPhase(
				ctx,
				&labvpn,
				laboratoryv1alpha1.LabVPNPhaseWaitingForInterface,
			); patchErr != nil {
				log.Error(patchErr, "patch phase WaitingForInterface")
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	cidr, err := netutil.SubnetForIndex(r.Cfg.VPNBaseNetwork, 24, labvpn.Spec.NetworkIndex)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("compute VPN CIDR: %w", err)
	}

	// Assign first host IP of the lab's /24 to the interface (idempotent).
	if err := netutil.AssignFirstHostIP(ifaceName, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("assign IP to %s: %w", ifaceName, err)
	}

	// DHCP: optional, only if pool exists.
	dhcpEnabled := r.dhcpPoolExists(ctx, labvpn.Spec.LabName, labvpn.Namespace)
	if dhcpEnabled {
		var lab laboratoryv1alpha1.Lab
		if err := r.Get(ctx, types.NamespacedName{Name: labvpn.Spec.LabName, Namespace: labvpn.Namespace}, &lab); err != nil {
			return ctrl.Result{}, fmt.Errorf("load lab DHCP settings: %w", err)
		}
		ranges, _, err := labdhcp.Settings(&lab, "vpn")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("VPN DHCP settings: %w", err)
		}
		gwIP := firstHostIP(cidr)
		if err := r.DHCP.Start(
			labvpn.Spec.LabName, dhcp.Config{
				Iface:   ifaceName,
				Subnet:  cidr,
				Gateway: gwIP,
				BindIP:  gwIP,
				Ranges:  ranges,
			},
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("start VPN DHCP for lab %s: %w", labvpn.Spec.LabName, err)
		}
	}

	if labvpn.Status.Phase != laboratoryv1alpha1.LabVPNPhaseReady {
		r.Recorder.Eventf(
			&labvpn, corev1.EventTypeNormal, labstatus.ReasonReady,
			"lab VPN routing ready on %s (%s)", ifaceName, cidr,
		)
	}
	newStatus := laboratoryv1alpha1.LabVPNStatus{
		Phase:       laboratoryv1alpha1.LabVPNPhaseReady,
		DHCPEnabled: dhcpEnabled,
		DHCPReady:   dhcpEnabled,
		Conditions:  labvpn.Status.Conditions,
	}
	labstatus.SetReady(&newStatus.Conditions, labvpn.Generation, true, labstatus.ReasonReady, "lab VPN routing ready")
	return ctrl.Result{}, r.patchStatus(ctx, &labvpn, newStatus)
}

func (r *LabVPNReconciler) reconcileDelete(ctx context.Context, labvpn *laboratoryv1alpha1.LabVPN) (
	ctrl.Result,
	error,
) {
	r.DHCP.Stop(labvpn.Spec.LabName)
	controllerutil.RemoveFinalizer(labvpn, names.FinalizerVPN)
	return ctrl.Result{}, r.Update(ctx, labvpn)
}

func (r *LabVPNReconciler) dhcpPoolExists(ctx context.Context, labName, namespace string) bool {
	var pool allocationv1alpha1.Pool
	err := r.Get(
		ctx, types.NamespacedName{
			Name:      fmt.Sprintf("dhcp-vpn-%s-0", labName),
			Namespace: namespace,
		}, &pool,
	)
	return err == nil
}

func (r *LabVPNReconciler) patchPhase(
	ctx context.Context,
	labvpn *laboratoryv1alpha1.LabVPN,
	phase laboratoryv1alpha1.LabVPNPhase,
) error {
	patch := client.MergeFrom(labvpn.DeepCopy())
	labvpn.Status.Phase = phase
	return r.Status().Patch(ctx, labvpn, patch)
}

func (r *LabVPNReconciler) patchStatus(
	ctx context.Context,
	labvpn *laboratoryv1alpha1.LabVPN,
	s laboratoryv1alpha1.LabVPNStatus,
) error {
	patch := client.MergeFrom(labvpn.DeepCopy())
	labvpn.Status = s
	return r.Status().Patch(ctx, labvpn, patch)
}

func (r *LabVPNReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabVPN{}).
		Complete(r)
}

func firstHostIP(cidr string) string {
	ip, _, _ := net.ParseCIDR(cidr)
	return netutil.NextIP(ip).String()
}
