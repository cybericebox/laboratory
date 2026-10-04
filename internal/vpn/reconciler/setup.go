//go:build linux

package reconciler

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/pkg/dhcp"
)

// Setup registers VPN controllers with mgr.
// wg and cfg must already be initialised (call vpn.InitServer first).
func Setup(ctx context.Context, mgr ctrl.Manager, wg *vpn.WGManager, ipt *vpn.IPTablesManager, cfg *vpn.Config) error {
	dhcpMgr := dhcp.NewManager()

	if err := (&LabGroupClientReconciler{
		Client:   mgr.GetClient(),
		WG:       wg,
		Cfg:      cfg,
		Recorder: mgr.GetEventRecorderFor("vpn-labgroupclient"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup LabGroupClientReconciler: %w", err)
	}
	if err := (&AccessReconciler{Client: mgr.GetClient(), IPT: ipt, Conntrack: vpn.NewConntrackRevoker()}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup LabGroup access reconciler: %w", err)
	}

	if err := (&LabVPNReconciler{
		Client:   mgr.GetClient(),
		WG:       wg,
		DHCP:     dhcpMgr,
		IPT:      ipt,
		Cfg:      cfg,
		Recorder: mgr.GetEventRecorderFor("labvpn"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup LabVPNReconciler: %w", err)
	}

	go RunStats(ctx, mgr.GetClient(), wg, cfg)
	go RunAccessStats(ctx, mgr.GetClient(), ipt, cfg)
	go RunFlowAccounting(ctx, mgr, cfg)

	return nil
}
