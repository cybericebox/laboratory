//go:build linux

package gateway

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"k8s.io/apimachinery/pkg/types"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/ovsnames"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

const (
	finalizerController = "cybericebox.com/controller"
	finalizerGateway    = "cybericebox.com/gateway"
)

type LabGatewayReconciler struct {
	client.Client
	DHCP *dhcp.Manager
	IPT  *IPTablesManager
	Cfg  *Config
}

func (r *LabGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("gateway").WithValues("labgateway", req.NamespacedName)

	var gw laboratoryv1alpha1.LabGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Main controller must have processed this first.
	if !controllerutil.ContainsFinalizer(&gw, finalizerController) {
		return ctrl.Result{}, nil
	}

	// Deletion path.
	if !gw.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&gw, finalizerGateway) {
			return r.reconcileDelete(ctx, &gw)
		}
		return ctrl.Result{}, nil
	}

	// Add own finalizer on first observation.
	if !controllerutil.ContainsFinalizer(&gw, finalizerGateway) {
		controllerutil.AddFinalizer(&gw, finalizerGateway)
		return ctrl.Result{}, r.Update(ctx, &gw)
	}

	// Wait for lab{N} interface to appear (created by node-agent via OVS).
	ifaceName := ovsnames.LabIfaceNameByIndex(gw.Spec.NetworkIndex)
	if _, err := netlink.LinkByName(ifaceName); err != nil {
		if gw.Status.Phase != laboratoryv1alpha1.LabGatewayPhaseWaitingForInterface {
			if patchErr := r.patchPhase(ctx, &gw, laboratoryv1alpha1.LabGatewayPhaseWaitingForInterface); patchErr != nil {
				log.Error(patchErr, "patch phase WaitingForInterface")
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Assign 10.192.N.1/24 to interface (idempotent).
	if err := netutil.AssignFirstHostIP(ifaceName, gw.Spec.CIDR); err != nil {
		return ctrl.Result{}, fmt.Errorf("assign IP to %s: %w", ifaceName, err)
	}

	// NAT: POSTROUTING MASQUERADE for this lab's subnet.
	if err := r.IPT.AddMasquerade(gw.Spec.CIDR); err != nil {
		return ctrl.Result{}, fmt.Errorf("add masquerade %s: %w", gw.Spec.CIDR, err)
	}

	// DHCP: optional, only if pool exists.
	dhcpEnabled := r.dhcpPoolExists(ctx, gw.Spec.LabName, gw.Namespace)
	if dhcpEnabled {
		gwIP := firstHostIP(gw.Spec.CIDR)
		if err := r.DHCP.Start(gw.Spec.LabName, dhcp.Config{
			Iface:   ifaceName,
			Subnet:  gw.Spec.CIDR,
			Gateway: gwIP,
			BindIP:  gwIP,
			DNS:     r.Cfg.DHCPDNS,
		}); err != nil {
			log.Error(err, "start DHCP", "lab", gw.Spec.LabName)
		}
	}

	return ctrl.Result{}, r.patchStatus(ctx, &gw, laboratoryv1alpha1.LabGatewayStatus{
		Phase:       laboratoryv1alpha1.LabGatewayPhaseReady,
		NATReady:    true,
		DHCPEnabled: dhcpEnabled,
		DHCPReady:   dhcpEnabled,
	})
}

func (r *LabGatewayReconciler) reconcileDelete(ctx context.Context, gw *laboratoryv1alpha1.LabGateway) (ctrl.Result, error) {
	r.DHCP.Stop(gw.Spec.LabName)
	if gw.Spec.CIDR != "" {
		r.IPT.DelMasquerade(gw.Spec.CIDR)
	}
	controllerutil.RemoveFinalizer(gw, finalizerGateway)
	return ctrl.Result{}, r.Update(ctx, gw)
}

func (r *LabGatewayReconciler) dhcpPoolExists(ctx context.Context, labName, namespace string) bool {
	var pool allocationv1alpha1.Pool
	err := r.Get(ctx, types.NamespacedName{
		Name:      fmt.Sprintf("dhcp-inet-%s-0", labName),
		Namespace: namespace,
	}, &pool)
	return err == nil
}

func (r *LabGatewayReconciler) patchPhase(ctx context.Context, gw *laboratoryv1alpha1.LabGateway, phase laboratoryv1alpha1.LabGatewayPhase) error {
	patch := client.MergeFrom(gw.DeepCopy())
	gw.Status.Phase = phase
	return r.Status().Patch(ctx, gw, patch)
}

func (r *LabGatewayReconciler) patchStatus(ctx context.Context, gw *laboratoryv1alpha1.LabGateway, s laboratoryv1alpha1.LabGatewayStatus) error {
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
