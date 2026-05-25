//go:build linux

package main

import (
	"context"
	"net"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/ovsnames"
)

const finalizerGateway = "cybericebox.com/gateway"

type LabGatewayReconciler struct {
	client.Client
	DHCP *DHCPManager
	IPT  *IPTablesManager
	Cfg  *Config
}

func (r *LabGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lab laboratoryv1alpha1.Lab
	if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lab.Spec.Internet.Enabled {
		return ctrl.Result{}, nil
	}

	if !lab.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lab)
	}

	cidr := lab.Status.Internet.CIDR
	if cidr == "" {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	ifaceName := ovsnames.LabGWIfaceName(lab.Name)
	if err := assignGatewayIP(ifaceName, cidr); err != nil {
		// Interface not yet created by node-agent; requeue.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if err := r.IPT.AddMasquerade(cidr); err != nil {
		return ctrl.Result{}, err
	}

	// Start DHCP server if configured and enabled.
	if ds := lab.Spec.Internet.DHCPServer; ds != nil && ds.Enabled {
		gwIP := nextIPFromCIDR(cidr)
		// Use DNS from spec if provided, fall back to 8.8.8.8.
		dnsIP := ds.DNS
		if dnsIP == "" {
			dnsIP = "8.8.8.8"
		}
		// Use subnet from spec if provided, fall back to the allocated CIDR.
		subnet := ds.Subnet
		if subnet == "" {
			subnet = cidr
		}
		// Use gateway from spec if provided, fall back to computed gateway IP.
		gw := ds.Gateway
		if gw == "" {
			gw = gwIP
		}
		if err := r.DHCP.Start(lab.Name, DHCPConfig{
			Iface:   ifaceName,
			Subnet:  subnet,
			Gateway: gw,
			DNS:     dnsIP,
		}); err != nil {
			ctrl.Log.WithName("gateway").Error(err, "start DHCP", "lab", lab.Name)
		}
	}

	lab.Status.Internet.Ready = true
	return ctrl.Result{}, r.Status().Update(ctx, &lab)
}

func (r *LabGatewayReconciler) reconcileDelete(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	r.DHCP.Stop(lab.Name)
	if lab.Status.Internet.CIDR != "" {
		r.IPT.DelMasquerade(lab.Status.Internet.CIDR)
	}
	controllerutil.RemoveFinalizer(lab, finalizerGateway)
	return ctrl.Result{}, r.Update(ctx, lab)
}

func (r *LabGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Lab{}).
		Complete(r)
}

// nextIPFromCIDR returns the first host IP in a CIDR as a string.
func nextIPFromCIDR(cidr string) string {
	ip, _, _ := net.ParseCIDR(cidr)
	return nextIP(ip).String()
}
