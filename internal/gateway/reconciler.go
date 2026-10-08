//go:build linux

package gateway

import (
	"context"
	"fmt"
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
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

type LabGatewayReconciler struct {
	client.Client
	DHCP     *dhcp.Manager
	IPT      *IPTablesManager
	Cfg      *Config
	Recorder record.EventRecorder
	applied  map[string]appliedNetwork
}

func (r *LabGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

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

	var parent laboratoryv1alpha1.Lab
	if err := r.Get(ctx, client.ObjectKey{Name: gw.Spec.LabName, Namespace: gw.Namespace}, &parent); err != nil {
		return r.dhcpFailure(ctx, &gw, err)
	}
	if parent.Spec.Lifecycle.IsStopped() {
		r.DHCP.Stop(gw.Spec.LabName)
		iface := names.LabIfaceNameByIndex(gw.Spec.NetworkIndex)
		if err := r.IPT.BlockLab(iface); err != nil {
			return ctrl.Result{}, err
		}
		r.IPT.DenyDHCP(iface)
		cidr, err := netutil.SubnetForIndex(r.Cfg.InetBaseNetwork, 24, gw.Spec.NetworkIndex)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.clearNetwork(appliedNetwork{Iface: iface, CIDR: cidr}); err != nil {
			return ctrl.Result{}, err
		}
		delete(r.applied, gw.Name)
		next := gw.Status
		next.Conditions = slices.Clone(next.Conditions)
		next.Phase = laboratoryv1alpha1.LabGatewayPhasePending
		next.DHCPReady = false
		next.NATReady = false
		labstatus.SetReady(&next.Conditions, gw.Generation, false, "LabStopped", "lab runtime is stopped")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.patchStatus(ctx, &gw, next)
	}
	dhcpEnabled, ranges, dns, dhcpErr := labdhcp.Desired(ctx, r.Client, gw.Namespace, gw.Spec.LabName, "internet")
	if dhcpErr != nil {
		return r.dhcpFailure(ctx, &gw, dhcpErr)
	}
	if r.applied == nil {
		r.applied = map[string]appliedNetwork{}
	}

	// Wait for lab{N} interface to appear (created by node-agent via OVS).
	ifaceName := names.LabIfaceNameByIndex(gw.Spec.NetworkIndex)
	link, linkErr := netlink.LinkByName(ifaceName)
	if linkErr != nil {
		r.DHCP.Stop(gw.Spec.LabName)
		if old, ok := r.applied[gw.Name]; ok {
			if err := r.clearNetwork(old); err != nil {
				return r.networkFailure(ctx, &gw, err)
			}
			delete(r.applied, gw.Name)
		}
		next := gw.Status
		next.Conditions = slices.Clone(next.Conditions)
		next.Phase = laboratoryv1alpha1.LabGatewayPhaseWaitingForInterface
		next.DHCPReady = false
		next.NATReady = false
		labstatus.SetReady(&next.Conditions, gw.Generation, false, labstatus.ReasonWaitingForInterface, "waiting for lab interface")
		if err := r.patchStatus(ctx, &gw, next); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	cidr, err := netutil.SubnetForIndex(r.Cfg.InetBaseNetwork, 24, gw.Spec.NetworkIndex)
	if err != nil {
		return r.networkFailure(ctx, &gw, fmt.Errorf("compute inet CIDR: %w", err))
	}

	desired := appliedNetwork{Iface: ifaceName, CIDR: cidr, LinkIndex: link.Attrs().Index, Hardware: link.Attrs().HardwareAddr.String()}
	previous, known := r.applied[gw.Name]
	if !known || previous.Iface != desired.Iface || previous.CIDR != desired.CIDR || previous.LinkIndex != desired.LinkIndex || previous.Hardware != desired.Hardware || !networkPresent(link, cidr) {
		if err := r.IPT.BlockLab(ifaceName); err != nil {
			return r.networkFailure(ctx, &gw, fmt.Errorf("close lab source gate: %w", err))
		}

		if known {
			r.DHCP.Stop(gw.Spec.LabName)
			if err := r.clearNetwork(previous); err != nil {
				return r.networkFailure(ctx, &gw, err)
			}
			delete(r.applied, gw.Name)
		}
		// Assign first host IP of the lab's /24 to the interface (idempotent).
		if err := netutil.AssignFirstHostIP(ifaceName, cidr); err != nil {
			return r.networkFailure(ctx, &gw, fmt.Errorf("assign IP to %s: %w", ifaceName, err))
		}

		// The lab may ping the pod's address on its own interface, and nothing else of the pod.
		if err := r.IPT.AllowPing(ifaceName, firstHostIP(cidr)); err != nil {
			return r.networkFailure(ctx, &gw, fmt.Errorf("allow ping of %s: %w", ifaceName, err))
		}

		// A lab may send only from its own subnet; installed before the lab can send anything through NAT.
		if err := r.IPT.AddAntiSpoof(ifaceName, cidr); err != nil {
			return r.networkFailure(ctx, &gw, fmt.Errorf("add anti-spoof rule for %s: %w", ifaceName, err))
		}

		// NAT: POSTROUTING MASQUERADE for this lab's subnet.
		if err := r.IPT.AddMasquerade(cidr); err != nil {
			return r.networkFailure(ctx, &gw, fmt.Errorf("add masquerade %s: %w", cidr, err))
		}

		if err := r.IPT.UnblockLab(ifaceName); err != nil {
			return r.networkFailure(ctx, &gw, fmt.Errorf("open secured lab source gate: %w", err))
		}
		r.applied[gw.Name] = desired
	}
	state := r.applied[gw.Name]
	if dhcpEnabled {
		gwIP := firstHostIP(cidr)
		// The pod drops everything addressed to itself from the lab side, DHCP on this interface excepted.
		if !state.DHCPKnown || !state.DHCP {
			if err := r.IPT.AllowDHCP(ifaceName); err != nil {
				return r.dhcpFailure(ctx, &gw, fmt.Errorf("open DHCP on %s: %w", ifaceName, err))
			}
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
			return r.dhcpFailure(ctx, &gw, fmt.Errorf("start DHCP for lab %s: %w", gw.Spec.LabName, err))
		}
	} else {
		r.DHCP.Stop(gw.Spec.LabName)
		if !state.DHCPKnown || state.DHCP {
			r.IPT.DenyDHCP(ifaceName)
		}
	}
	state.DHCPKnown = true
	state.DHCP = dhcpEnabled
	r.applied[gw.Name] = state
	if dhcpEnabled && !r.DHCP.Healthy(gw.Spec.LabName) {
		return r.dhcpFailure(ctx, &gw, fmt.Errorf("DHCP socket is unavailable"))
	}

	if r.Recorder != nil && gw.Status.Phase != laboratoryv1alpha1.LabGatewayPhaseReady {
		r.Recorder.Eventf(
			&gw, corev1.EventTypeNormal, labstatus.ReasonReady,
			"lab internet gateway ready on %s (NAT active)", ifaceName,
		)
	}
	newStatus := laboratoryv1alpha1.LabGatewayStatus{
		Phase:       laboratoryv1alpha1.LabGatewayPhaseReady,
		NATReady:    true,
		DHCPEnabled: dhcpEnabled,
		DHCPReady:   dhcpEnabled && r.DHCP.Healthy(gw.Spec.LabName),
		Conditions:  slices.Clone(gw.Status.Conditions),
	}
	labstatus.SetReady(&newStatus.Conditions, gw.Generation, true, labstatus.ReasonReady, "lab internet gateway ready")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, r.patchStatus(ctx, &gw, newStatus)
}

func (r *LabGatewayReconciler) reconcileDelete(ctx context.Context, gw *laboratoryv1alpha1.LabGateway) (
	ctrl.Result,
	error,
) {
	if err := r.IPT.BlockLab(names.LabIfaceNameByIndex(gw.Spec.NetworkIndex)); err != nil {
		return ctrl.Result{}, err
	}
	r.DHCP.Drop(gw.Spec.LabName)
	r.IPT.DenyDHCP(names.LabIfaceNameByIndex(gw.Spec.NetworkIndex))
	if cidr, err := netutil.SubnetForIndex(r.Cfg.InetBaseNetwork, 24, gw.Spec.NetworkIndex); err == nil {
		r.IPT.DelMasquerade(cidr)
		r.IPT.DenyPing(names.LabIfaceNameByIndex(gw.Spec.NetworkIndex), firstHostIP(cidr))
		r.IPT.DelAntiSpoof(names.LabIfaceNameByIndex(gw.Spec.NetworkIndex), cidr)
	}
	if old, ok := r.applied[gw.Name]; ok {
		if err := r.clearNetwork(old); err != nil {
			return ctrl.Result{}, err
		}
		delete(r.applied, gw.Name)
	}
	controllerutil.RemoveFinalizer(gw, names.FinalizerGateway)
	return ctrl.Result{}, r.Update(ctx, gw)
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
	if reflect.DeepEqual(gw.Status, s) {
		return nil
	}
	patch := client.MergeFrom(gw.DeepCopy())
	gw.Status = s
	return r.Status().Patch(ctx, gw, patch)
}

func (r *LabGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b, err := labdhcp.WatchDHCP(mgr, ctrl.NewControllerManagedBy(mgr).For(&laboratoryv1alpha1.LabGateway{}), r.Client, r.DHCP, r.Cfg.Namespace, "internet")
	if err != nil {
		return err
	}
	return b.Complete(reconcileutil.Quiet(r))
}
func (r *LabGatewayReconciler) clearNetwork(old appliedNetwork) error {
	if err := r.IPT.BlockLab(old.Iface); err != nil {
		return err
	}
	r.IPT.DenyDHCP(old.Iface)
	r.IPT.DenyPing(old.Iface, firstHostIP(old.CIDR))
	removeNetworkAddress(old)
	r.IPT.DelMasquerade(old.CIDR)
	r.IPT.DelAntiSpoof(old.Iface, old.CIDR)
	return nil
}
func (r *LabGatewayReconciler) dhcpFailure(ctx context.Context, gw *laboratoryv1alpha1.LabGateway, err error) (ctrl.Result, error) {
	r.DHCP.Stop(gw.Spec.LabName)
	iface := names.LabIfaceNameByIndex(gw.Spec.NetworkIndex)
	if r.IPT != nil {
		r.IPT.DenyDHCP(iface)
	}
	if old, ok := r.applied[gw.Name]; ok {
		old.DHCPKnown = false
		r.applied[gw.Name] = old
	}
	next := gw.Status
	next.Conditions = slices.Clone(next.Conditions)
	next.Phase = laboratoryv1alpha1.LabGatewayPhaseConfiguring
	next.DHCPReady = false
	if _, ok := r.applied[gw.Name]; ok {
		next.NATReady = true
	}
	labstatus.SetReady(&next.Conditions, gw.Generation, false, "DHCPFailed", err.Error())
	if patchErr := r.patchStatus(ctx, gw, next); patchErr != nil {
		return ctrl.Result{}, fmt.Errorf("%v; patch DHCP status: %w", err, patchErr)
	}
	return ctrl.Result{}, err
}

func firstHostIP(cidr string) string {
	ip, _, _ := net.ParseCIDR(cidr)
	return netutil.NextIP(ip).String()
}

func (r *LabGatewayReconciler) networkFailure(ctx context.Context, obj *laboratoryv1alpha1.LabGateway, err error) (ctrl.Result, error) {
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
	next.Phase = laboratoryv1alpha1.LabGatewayPhaseConfiguring
	next.NATReady = false
	labstatus.SetReady(&next.Conditions, obj.Generation, false, "NetworkFailed", err.Error())
	if patchErr := r.patchStatus(ctx, obj, next); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{}, err
}
