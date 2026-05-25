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
		dnsIP := ds.DNS
		if dnsIP == "" {
			dnsIP = "8.8.8.8"
		}
		subnet := ds.Subnet
		if subnet == "" {
			subnet = cidr
		}
		gw := ds.Gateway
		if gw == "" {
			gw = gwIP
		}
		if err := r.DHCP.Start(lab.Name, DHCPConfig{
			Iface:    ifaceName,
			Subnet:   subnet,
			Gateway:  gw,
			DNS:      dnsIP,
			Range:    ds.Range,
			Reserved: collectStaticIPs(&lab, subnet),
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

// collectStaticIPs returns IPs of all statically-addressed device interfaces
// whose IP falls inside subnetCIDR. Spec §8 — static devices must be excluded
// from DHCP allocation to avoid duplicate-IP races.
func collectStaticIPs(lab *laboratoryv1alpha1.Lab, subnetCIDR string) []net.IP {
	_, subnet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, d := range lab.Spec.Devices {
		for _, iface := range d.Interfaces {
			if iface.Addr.Type != laboratoryv1alpha1.AddrTypeStatic {
				continue
			}
			ip, _, err := net.ParseCIDR(iface.Addr.IP)
			if err != nil {
				// Allow bare IPs too (without prefix).
				ip = net.ParseIP(iface.Addr.IP)
				if ip == nil {
					continue
				}
			}
			if subnet.Contains(ip) {
				out = append(out, ip)
			}
		}
	}
	return out
}
