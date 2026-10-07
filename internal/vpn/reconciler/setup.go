//go:build linux

package reconciler

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"time"
)

// Setup registers VPN controllers with mgr.
// wg and cfg must already be initialised (call vpn.InitServer first).
func Setup(ctx context.Context, mgr ctrl.Manager, wg *vpn.WGManager, ipt *vpn.IPTablesManager, cfg *vpn.Config) error {
	dhcpMgr := dhcp.NewManager()
	reporter, source, err := prepareFlowAccounting(ctx, mgr, cfg, ipt)
	if err != nil {
		return fmt.Errorf("resume VPN traffic: %w", err)
	}

	if err := (&LabGroupClientReconciler{
		Client:   mgr.GetClient(),
		WG:       wg,
		Cfg:      cfg,
		Recorder: mgr.GetEventRecorderFor("vpn-labgroupclient"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup LabGroupClientReconciler: %w", err)
	}
	if err := (&AccessReconciler{Client: mgr.GetClient(), IPT: ipt, RequireInitialRetirement: true, InitialBindings: ipt.KnownBindingIDs(), Counters: func() map[string]vpn.TrafficCounter { return accessTotals(reporter.Collector.Snapshot(time.Now())) }, Conntrack: vpn.NewConntrackRevoker()}).SetupWithManager(mgr); err != nil {
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
	if err := mgr.Add(manager.RunnableFunc(func(runCtx context.Context) error {
		defer source.Close()
		reporter.Run(runCtx, flowacct.DefaultPollEvery, flowacct.DefaultReportEvery, func(err error) { ctrl.Log.WithName("flowacct").Error(err, "flow accounting") })
		return nil
	})); err != nil {
		source.Close()
		return fmt.Errorf("register VPN traffic reporter: %w", err)
	}

	return nil
}
