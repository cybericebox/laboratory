//go:build linux

package gateway

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
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

type LabGatewayReconciler struct {
	client.Client
	DHCP     *dhcp.Manager
	IPT      *IPTablesManager
	Cfg      *Config
	Recorder record.EventRecorder
}

func (r *LabGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("gateway").WithValues("labgateway", req.NamespacedName)

	var gw laboratoryv1alpha1.LabGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path — checked before the guard so cleanup runs even after
	// main controller removes FinalizerController to unblock this reconciler.
	if !gw.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&gw, names.FinalizerGateway) {
			return r.reconcileDelete(ctx, &gw)
		}
		return ctrl.Result{}, nil
	}

	// Main controller must have processed this first.
	if !controllerutil.ContainsFinalizer(&gw, names.FinalizerController) {
		return ctrl.Result{}, nil
	}

	// Add own finalizer on first observation.
	if !controllerutil.ContainsFinalizer(&gw, names.FinalizerGateway) {
		controllerutil.AddFinalizer(&gw, names.FinalizerGateway)
		return ctrl.Result{}, r.Update(ctx, &gw)
	}

	// Wait for lab{N} interface to appear (created by node-agent via OVS).
	ifaceName := names.LabIfaceNameByIndex(gw.Spec.NetworkIndex)
	if _, err := netlink.LinkByName(ifaceName); err != nil {
		if gw.Status.Phase != laboratoryv1alpha1.LabGatewayPhaseWaitingForInterface {
			r.Recorder.Eventf(
				&gw, corev1.EventTypeWarning, labstatus.ReasonWaitingForInterface,
				"waiting for OVS interface %q (node-agent has not attached it yet)", ifaceName,
			)
			if patchErr := r.patchPhase(
				ctx,
				&gw,
				laboratoryv1alpha1.LabGatewayPhaseWaitingForInterface,
			); patchErr != nil {
				log.Error(patchErr, "patch phase WaitingForInterface")
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	cidr, err := netutil.SubnetForIndex(r.Cfg.InetBaseNetwork, 24, gw.Spec.NetworkIndex)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("compute inet CIDR: %w", err)
	}

	// Assign first host IP of the lab's /24 to the interface (idempotent).
	if err := netutil.AssignFirstHostIP(ifaceName, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("assign IP to %s: %w", ifaceName, err)
	}

	// A lab may send only from its own subnet; installed before the lab can send anything through NAT.
	if err := r.IPT.AddAntiSpoof(ifaceName, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("add anti-spoof rule for %s: %w", ifaceName, err)
	}

	// NAT: POSTROUTING MASQUERADE for this lab's subnet.
	if err := r.IPT.AddMasquerade(cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("add masquerade %s: %w", cidr, err)
	}

	// DHCP: optional, only if pool exists.
	dhcpEnabled := r.dhcpPoolExists(ctx, gw.Spec.LabName, gw.Namespace)
	if dhcpEnabled {
		var lab laboratoryv1alpha1.Lab
		if err := r.Get(ctx, types.NamespacedName{Name: gw.Spec.LabName, Namespace: gw.Namespace}, &lab); err != nil {
			return ctrl.Result{}, fmt.Errorf("load lab DHCP settings: %w", err)
		}
		ranges, dns, err := labdhcp.Settings(&lab, "internet")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("internet DHCP settings: %w", err)
		}
		gwIP := firstHostIP(cidr)
		// The pod drops everything addressed to itself from the lab side, DHCP on this interface excepted.
		if err := r.IPT.AllowDHCP(ifaceName); err != nil {
			return ctrl.Result{}, fmt.Errorf("open DHCP on %s: %w", ifaceName, err)
		}
		if err := r.DHCP.Start(
			gw.Spec.LabName, dhcp.Config{
				Iface:   ifaceName,
				Subnet:  cidr,
				Gateway: gwIP,
				BindIP:  gwIP,
				DNS:     dns,
				Ranges:  ranges,
			},
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("start internet DHCP for lab %s: %w", gw.Spec.LabName, err)
		}
	} else {
		r.IPT.DenyDHCP(ifaceName)
	}

	if gw.Status.Phase != laboratoryv1alpha1.LabGatewayPhaseReady {
		r.Recorder.Eventf(
			&gw, corev1.EventTypeNormal, labstatus.ReasonReady,
			"lab internet gateway ready on %s (NAT active)", ifaceName,
		)
	}
	newStatus := laboratoryv1alpha1.LabGatewayStatus{
		Phase:       laboratoryv1alpha1.LabGatewayPhaseReady,
		NATReady:    true,
		DHCPEnabled: dhcpEnabled,
		DHCPReady:   dhcpEnabled,
		Conditions:  gw.Status.Conditions,
	}
	labstatus.SetReady(&newStatus.Conditions, gw.Generation, true, labstatus.ReasonReady, "lab internet gateway ready")
	return ctrl.Result{}, r.patchStatus(ctx, &gw, newStatus)
}

func (r *LabGatewayReconciler) reconcileDelete(ctx context.Context, gw *laboratoryv1alpha1.LabGateway) (
	ctrl.Result,
	error,
) {
	r.DHCP.Stop(gw.Spec.LabName)
	r.IPT.DenyDHCP(names.LabIfaceNameByIndex(gw.Spec.NetworkIndex))
	if cidr, err := netutil.SubnetForIndex(r.Cfg.InetBaseNetwork, 24, gw.Spec.NetworkIndex); err == nil {
		r.IPT.DelMasquerade(cidr)
		r.IPT.DelAntiSpoof(names.LabIfaceNameByIndex(gw.Spec.NetworkIndex), cidr)
	}
	controllerutil.RemoveFinalizer(gw, names.FinalizerGateway)
	return ctrl.Result{}, r.Update(ctx, gw)
}

func (r *LabGatewayReconciler) dhcpPoolExists(ctx context.Context, labName, namespace string) bool {
	var pool allocationv1alpha1.Pool
	err := r.Get(
		ctx, types.NamespacedName{
			Name:      fmt.Sprintf("dhcp-inet-%s-0", labName),
			Namespace: namespace,
		}, &pool,
	)
	return err == nil
}

func (r *LabGatewayReconciler) patchPhase(
	ctx context.Context,
	gw *laboratoryv1alpha1.LabGateway,
	phase laboratoryv1alpha1.LabGatewayPhase,
) error {
	patch := client.MergeFrom(gw.DeepCopy())
	gw.Status.Phase = phase
	return r.Status().Patch(ctx, gw, patch)
}

func (r *LabGatewayReconciler) patchStatus(
	ctx context.Context,
	gw *laboratoryv1alpha1.LabGateway,
	s laboratoryv1alpha1.LabGatewayStatus,
) error {
	patch := client.MergeFrom(gw.DeepCopy())
	gw.Status = s
	return r.Status().Patch(ctx, gw, patch)
}

func (r *LabGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGateway{}).
		Complete(r)
}

func firstHostIP(cidr string) string {
	ip, _, _ := net.ParseCIDR(cidr)
	return netutil.NextIP(ip).String()
}
