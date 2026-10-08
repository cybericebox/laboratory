//go:build linux

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"reflect"
	"slices"
	"time"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labdhcp"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
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
	IPT      *vpn.IPTablesManager
	Cfg      *vpn.Config
	Recorder record.EventRecorder
	applied  map[string]appliedNetwork
}

func (r *LabVPNReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

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

	var parent laboratoryv1alpha1.Lab
	if err := r.Get(ctx, client.ObjectKey{Name: labvpn.Spec.LabName, Namespace: labvpn.Namespace}, &parent); err != nil {
		return r.dhcpFailure(ctx, &labvpn, err)
	}
	if parent.Spec.Lifecycle.IsStopped() {
		r.DHCP.Stop(labvpn.Spec.LabName)
		iface := names.LabIfaceNameByIndex(labvpn.Spec.NetworkIndex)
		r.IPT.DenyDHCP(iface)
		if cidr, err := netutil.SubnetForIndex(r.Cfg.VPNBaseNetwork, 24, labvpn.Spec.NetworkIndex); err == nil {
			r.IPT.DenyPing(iface, firstHostIP(cidr))
		}
		if old, ok := r.applied[labvpn.Name]; ok {
			r.clearNetwork(old)
			delete(r.applied, labvpn.Name)
		}
		next := labvpn.Status
		next.Conditions = slices.Clone(next.Conditions)
		next.Phase = laboratoryv1alpha1.LabVPNPhasePending
		next.DHCPReady = false
		labstatus.SetReady(&next.Conditions, labvpn.Generation, false, "LabStopped", "lab runtime is stopped")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.patchStatus(ctx, &labvpn, next)
	}
	dhcpEnabled, ranges, _, dhcpErr := labdhcp.Desired(ctx, r.Client, labvpn.Namespace, labvpn.Spec.LabName, "vpn")
	if dhcpErr != nil {
		return r.dhcpFailure(ctx, &labvpn, dhcpErr)
	}
	if r.applied == nil {
		r.applied = map[string]appliedNetwork{}
	}

	// Wait for lab{N} interface (created by node-agent via OVS).
	ifaceName := names.LabIfaceNameByIndex(labvpn.Spec.NetworkIndex)
	link, linkErr := netlink.LinkByName(ifaceName)
	if linkErr != nil {
		r.DHCP.Stop(labvpn.Spec.LabName)
		if old, ok := r.applied[labvpn.Name]; ok {
			r.clearNetwork(old)
			delete(r.applied, labvpn.Name)
		}
		next := labvpn.Status
		next.Conditions = slices.Clone(next.Conditions)
		next.Phase = laboratoryv1alpha1.LabVPNPhaseWaitingForInterface
		next.DHCPReady = false
		labstatus.SetReady(&next.Conditions, labvpn.Generation, false, labstatus.ReasonWaitingForInterface, "waiting for lab interface")
		if err := r.patchStatus(ctx, &labvpn, next); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	cidr, err := netutil.SubnetForIndex(r.Cfg.VPNBaseNetwork, 24, labvpn.Spec.NetworkIndex)
	if err != nil {
		return r.networkFailure(ctx, &labvpn, fmt.Errorf("compute VPN CIDR: %w", err))
	}

	desired := appliedNetwork{Iface: ifaceName, CIDR: cidr, LinkIndex: link.Attrs().Index, Hardware: link.Attrs().HardwareAddr.String()}
	previous, known := r.applied[labvpn.Name]
	if !known || previous.Iface != desired.Iface || previous.CIDR != desired.CIDR || previous.LinkIndex != desired.LinkIndex || previous.Hardware != desired.Hardware || !networkPresent(link, cidr) {
		if known {
			r.DHCP.Stop(labvpn.Spec.LabName)
			r.clearNetwork(previous)
			delete(r.applied, labvpn.Name)
		}
		// Assign first host IP of the lab's /24 to the interface (idempotent).
		if err := netutil.AssignFirstHostIP(ifaceName, cidr); err != nil {
			return r.networkFailure(ctx, &labvpn, fmt.Errorf("assign IP to %s: %w", ifaceName, err))
		}

		// The lab may ping the pod's address on its own interface, and nothing else of the pod.
		if err := r.IPT.AllowPing(ifaceName, firstHostIP(cidr)); err != nil {
			return r.networkFailure(ctx, &labvpn, fmt.Errorf("allow ping of %s: %w", ifaceName, err))
		}

		r.applied[labvpn.Name] = desired
	}
	state := r.applied[labvpn.Name]
	if dhcpEnabled {
		gwIP := firstHostIP(cidr)
		// The pod drops everything addressed to itself from the lab side, DHCP on this interface excepted.
		if !state.DHCPKnown || !state.DHCP {
			if err := r.IPT.AllowDHCP(ifaceName); err != nil {
				return r.dhcpFailure(ctx, &labvpn, fmt.Errorf("open DHCP on %s: %w", ifaceName, err))
			}
		}
		if err := r.DHCP.Start(
			labvpn.Spec.LabName, dhcp.Config{
				Iface:   ifaceName,
				Subnet:  cidr,
				Gateway: gwIP,
				BindIP:  gwIP,
				Ranges:  ranges,
			},
		); err != nil {
			return r.dhcpFailure(ctx, &labvpn, fmt.Errorf("start DHCP for lab %s: %w", labvpn.Spec.LabName, err))
		}
	} else {
		r.DHCP.Stop(labvpn.Spec.LabName)
		if !state.DHCPKnown || state.DHCP {
			r.IPT.DenyDHCP(ifaceName)
		}
	}
	state.DHCPKnown = true
	state.DHCP = dhcpEnabled
	r.applied[labvpn.Name] = state
	if dhcpEnabled && !r.DHCP.Healthy(labvpn.Spec.LabName) {
		return r.dhcpFailure(ctx, &labvpn, fmt.Errorf("DHCP socket is unavailable"))
	}

	if r.Recorder != nil && labvpn.Status.Phase != laboratoryv1alpha1.LabVPNPhaseReady {
		r.Recorder.Eventf(
			&labvpn, corev1.EventTypeNormal, labstatus.ReasonReady,
			"lab VPN routing ready on %s (%s)", ifaceName, cidr,
		)
	}
	newStatus := laboratoryv1alpha1.LabVPNStatus{
		Phase:       laboratoryv1alpha1.LabVPNPhaseReady,
		DHCPEnabled: dhcpEnabled,
		DHCPReady:   dhcpEnabled && r.DHCP.Healthy(labvpn.Spec.LabName),
		Conditions:  slices.Clone(labvpn.Status.Conditions),
	}
	labstatus.SetReady(&newStatus.Conditions, labvpn.Generation, true, labstatus.ReasonReady, "lab VPN routing ready")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, r.patchStatus(ctx, &labvpn, newStatus)
}

func (r *LabVPNReconciler) reconcileDelete(ctx context.Context, labvpn *laboratoryv1alpha1.LabVPN) (
	ctrl.Result,
	error,
) {
	r.DHCP.Drop(labvpn.Spec.LabName)
	ifaceName := names.LabIfaceNameByIndex(labvpn.Spec.NetworkIndex)
	r.IPT.DenyDHCP(ifaceName)
	if cidr, err := netutil.SubnetForIndex(r.Cfg.VPNBaseNetwork, 24, labvpn.Spec.NetworkIndex); err == nil {
		r.IPT.DenyPing(ifaceName, firstHostIP(cidr))
	}
	if old, ok := r.applied[labvpn.Name]; ok {
		r.clearNetwork(old)
		delete(r.applied, labvpn.Name)
	}
	controllerutil.RemoveFinalizer(labvpn, names.FinalizerVPN)
	return ctrl.Result{}, r.Update(ctx, labvpn)
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
	// AccessReconciler owns runtime and accessFence independently. Never include
	// those foreign fields in a network-status replacement, even from stale cache.
	s.Runtime = labvpn.Status.Runtime
	s.AccessFence = labvpn.Status.AccessFence
	if reflect.DeepEqual(labvpn.Status, s) {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"status": map[string]any{"phase": s.Phase, "dhcpEnabled": s.DHCPEnabled, "dhcpReady": s.DHCPReady, "conditions": s.Conditions}})
	if err != nil {
		return err
	}
	return r.Status().Patch(ctx, labvpn, client.RawPatch(types.MergePatchType, raw))
}

func (r *LabVPNReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b, err := labdhcp.WatchDHCP(mgr, ctrl.NewControllerManagedBy(mgr).For(&laboratoryv1alpha1.LabVPN{}), r.Client, r.DHCP, r.Cfg.Namespace, "vpn")
	if err != nil {
		return err
	}
	return b.Complete(reconcileutil.Quiet(r))
}
func (r *LabVPNReconciler) clearNetwork(old appliedNetwork) {
	r.IPT.DenyDHCP(old.Iface)
	r.IPT.DenyPing(old.Iface, firstHostIP(old.CIDR))
	removeNetworkAddress(old)

}
func (r *LabVPNReconciler) dhcpFailure(ctx context.Context, labvpn *laboratoryv1alpha1.LabVPN, err error) (ctrl.Result, error) {
	r.DHCP.Stop(labvpn.Spec.LabName)
	iface := names.LabIfaceNameByIndex(labvpn.Spec.NetworkIndex)
	if r.IPT != nil {
		r.IPT.DenyDHCP(iface)
	}
	if old, ok := r.applied[labvpn.Name]; ok {
		old.DHCPKnown = false
		r.applied[labvpn.Name] = old
	}
	next := labvpn.Status
	next.Conditions = slices.Clone(next.Conditions)
	next.Phase = laboratoryv1alpha1.LabVPNPhaseConfiguring
	next.DHCPReady = false
	labstatus.SetReady(&next.Conditions, labvpn.Generation, false, "DHCPFailed", err.Error())
	if patchErr := r.patchStatus(ctx, labvpn, next); patchErr != nil {
		return ctrl.Result{}, fmt.Errorf("%v; patch DHCP status: %w", err, patchErr)
	}
	return ctrl.Result{}, err
}

func firstHostIP(cidr string) string {
	ip, _, _ := net.ParseCIDR(cidr)
	return netutil.NextIP(ip).String()
}

func (r *LabVPNReconciler) networkFailure(ctx context.Context, obj *laboratoryv1alpha1.LabVPN, err error) (ctrl.Result, error) {
	r.DHCP.Stop(obj.Spec.LabName)
	if r.IPT != nil {
		r.IPT.DenyDHCP(names.LabIfaceNameByIndex(obj.Spec.NetworkIndex))
	}
	if old, ok := r.applied[obj.Name]; ok {
		old.DHCPKnown = false
		r.applied[obj.Name] = old
	}

	next := obj.Status
	next.Conditions = slices.Clone(next.Conditions)
	next.DHCPReady = false
	next.Phase = laboratoryv1alpha1.LabVPNPhaseConfiguring
	labstatus.SetReady(&next.Conditions, obj.Generation, false, "NetworkFailed", err.Error())
	if patchErr := r.patchStatus(ctx, obj, next); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{}, err
}
