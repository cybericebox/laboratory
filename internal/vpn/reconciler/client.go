//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn"
)

const finalizerVPNPeer = "cybericebox.com/vpn-peer"

// LabGroupClientReconciler manages WireGuard peers for LabGroupClient resources.
type LabGroupClientReconciler struct {
	client.Client
	WG  *vpn.WGManager
	Cfg *vpn.Config
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

// RunStats periodically reads WireGuard peer stats and writes them to LabGroupClient status.
func RunStats(ctx context.Context, c client.Client, wg *vpn.WGManager, cfg *vpn.Config) {
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
