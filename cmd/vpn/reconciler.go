//go:build linux

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/ovsnames"
)

const (
	finalizerVPNPeer = "cybericebox.com/vpn-peer"
	finalizerVPN     = "cybericebox.com/vpn"
)

// LabGroupClientReconciler watches LabGroupClient and manages WireGuard peers.
type LabGroupClientReconciler struct {
	client.Client
	WG  *WGManager
	Cfg *Config
}

func (r *LabGroupClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lgc laboratoryv1alpha1.LabGroupClient
	if err := r.Get(ctx, req.NamespacedName, &lgc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lgc.DeletionTimestamp.IsZero() {
		if lgc.Spec.PublicKey != "" {
			_ = r.WG.RemovePeer(lgc.Spec.PublicKey)
		}
		controllerutil.RemoveFinalizer(&lgc, finalizerVPNPeer)
		return ctrl.Result{}, r.Update(ctx, &lgc)
	}

	if !controllerutil.ContainsFinalizer(&lgc, finalizerVPNPeer) {
		controllerutil.AddFinalizer(&lgc, finalizerVPNPeer)
		if err := r.Update(ctx, &lgc); err != nil {
			return ctrl.Result{}, err
		}
	}

	pubKey := lgc.Spec.PublicKey
	assignedIP := lgc.Status.AssignedIP
	if pubKey == "" || assignedIP == "" {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if err := r.WG.AddPeer(pubKey, assignedIP); err != nil {
		return ctrl.Result{}, fmt.Errorf("add WG peer %s: %w", lgc.Name, err)
	}
	return ctrl.Result{}, nil
}

func (r *LabGroupClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroupClient{}).
		Complete(r)
}

// LabVPNReconciler watches Lab and manages per-lab WireGuard routing.
type LabVPNReconciler struct {
	client.Client
	WG  *WGManager
	Cfg *Config
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

	// Assign 10.{vpn}.N.1/24 to the lab interface so the kernel routes
	// VPN traffic into the lab segment. Without this the route below
	// is dead-end and packets to 10.{vpn}.N.0/24 are not forwarded.
	if err := assignGatewayIP(link, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("assign gateway IP for %s: %w", cidr, err)
	}

	if err := addLabRoute(link, cidr); err != nil {
		return ctrl.Result{}, fmt.Errorf("add route for %s: %w", cidr, err)
	}

	lab.Status.VPN.Ready = true
	return ctrl.Result{}, r.Status().Update(ctx, &lab)
}

func (r *LabVPNReconciler) reconcileDelete(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	if lab.Status.VPN.CIDR != "" {
		if err := delLabRoute(lab.Status.VPN.CIDR); err != nil {
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

// runStats reads WireGuard peer stats and updates LabGroupClient.Status.Statistics.
func runStats(ctx context.Context, c client.Client, wg *WGManager, cfg *Config) {
	ticker := time.NewTicker(cfg.StatsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dev, err := wg.Device()
			if err != nil {
				continue
			}
			var lgcList laboratoryv1alpha1.LabGroupClientList
			if err := c.List(ctx, &lgcList, client.InNamespace(cfg.Namespace)); err != nil {
				continue
			}
			for _, peer := range dev.Peers {
				pubKey := peer.PublicKey.String()
				for i := range lgcList.Items {
					if lgcList.Items[i].Spec.PublicKey == pubKey {
						lgcList.Items[i].Status.Statistics = laboratoryv1alpha1.LabGroupClientStatistics{
							LastHandshake: metav1.NewTime(peer.LastHandshakeTime),
							RxBytes:       peer.ReceiveBytes,
							TxBytes:       peer.TransmitBytes,
						}
						_ = c.Status().Update(ctx, &lgcList.Items[i])
						break
					}
				}
			}
		}
	}
}
