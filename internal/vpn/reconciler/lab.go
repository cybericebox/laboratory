//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/ovsnames"
	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

const finalizerVPN = "cybericebox.com/vpn"

// LabVPNReconciler manages per-lab WireGuard routing and optional DHCP.
type LabVPNReconciler struct {
	client.Client
	WG   *vpn.WGManager
	DHCP *dhcp.Manager
	Cfg  *vpn.Config
}

func (r *LabVPNReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lab laboratoryv1alpha1.Lab
	if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lab.Spec.VPN.Enabled {
		return ctrl.Result{}, nil
	}

	if !lab.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lab)
	}

	cidr := lab.Status.VPN.CIDR
	if cidr == "" {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	ifaceName := ovsnames.LabIfaceName(lab.Name)
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		// Interface not yet created by node-agent; requeue.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Assign first host IP so the kernel routes VPN traffic into the lab segment.
	if err := netutil.AssignFirstHostIPToLink(link, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("assign gateway IP for %s: %w", cidr, err)
	}

	if err := vpn.AddLabRoute(link, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("add route for %s: %w", cidr, err)
	}

	if ds := lab.Spec.VPN.DHCPServer; ds != nil && ds.Enabled {
		if err := r.DHCP.Start(lab.Name, vpnDHCPConfig(cidr, ifaceName)); err != nil {
			ctrl.Log.WithName("vpn").Error(err, "start DHCP", "lab", lab.Name)
		}
	}

	lab.Status.VPN.Ready = true
	return ctrl.Result{}, r.Status().Update(ctx, &lab)
}

func (r *LabVPNReconciler) reconcileDelete(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	r.DHCP.Stop(lab.Name)
	if lab.Status.VPN.CIDR != "" {
		if err := vpn.DelLabRoute(lab.Status.VPN.CIDR); err != nil {
			ctrl.Log.WithName("vpn").Error(err, "delete lab route", "cidr", lab.Status.VPN.CIDR)
		}
	}
	controllerutil.RemoveFinalizer(lab, finalizerVPN)
	return ctrl.Result{}, r.Update(ctx, lab)
}

func (r *LabVPNReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Lab{}).
		Complete(r)
}

func vpnDHCPConfig(cidr, iface string) dhcp.Config {
	ip, _, _ := net.ParseCIDR(cidr)
	return dhcp.Config{
		Iface:   iface,
		Subnet:  cidr,
		Gateway: netutil.NextIP(ip).String(),
	}
}
