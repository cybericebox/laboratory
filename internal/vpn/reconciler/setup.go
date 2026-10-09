//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/util/uuid"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"time"

	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	"github.com/cybericebox/laboratory/pkg/dhcp"
)

// Setup registers VPN controllers with mgr.
// wg and cfg must already be initialised (call vpn.InitServer first).
func Setup(ctx context.Context, mgr ctrl.Manager, wg *vpn.WGManager, ipt *vpn.IPTablesManager, cfg *vpn.Config) error {
	podName, err := os.Hostname()
	if err != nil {
		return err
	}
	runtimeID, err := awaitVPNRuntime(ctx, mgr.GetAPIReader(), cfg.Namespace, podName, string(uuid.NewUUID()))
	if err != nil {
		return fmt.Errorf("identify VPN boot: %w", err)
	}
	access := &AccessReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Runtime: runtimeID, GroupUID: os.Getenv("GROUP_UID"), IPT: ipt, RequireInitialRetirement: true, InitialBindings: ipt.KnownBindingIDs(), Conntrack: vpn.NewConntrackRevoker()}
	if access.GroupUID == "" {
		return fmt.Errorf("current VPN group UID is unknown")
	}
	if err := access.initializeCurrentBoot(ctx, cfg.Namespace); err != nil {
		return fmt.Errorf("publish independent current VPN boot: %w", err)
	}
	if err := access.publishBoot(ctx, cfg.Namespace); err != nil {
		return fmt.Errorf("publish VPN boot before kernel reconciliation: %w", err)
	}
	dhcpMgr := dhcp.NewManager()
	reporter, source, err := prepareFlowAccounting(ctx, mgr, cfg, ipt, runtimeID.BootID)
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
	access.Counters = func() map[string]vpn.TrafficCounter { return accessTotals(reporter.Collector.Snapshot(time.Now())) }
	if err := access.SetupWithManager(mgr); err != nil {
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
