//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"github.com/cybericebox/laboratory/internal/names"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"net/netip"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

// prepareFlowAccounting resumes the single ledger before any controller can
// replace surviving chains. Read/API failure leaves the startup gate closed.
func prepareFlowAccounting(ctx context.Context, mgr ctrl.Manager, cfg *vpn.Config, ipt *vpn.IPTablesManager) (*flowacct.Reporter, *flowacct.Conntrack, error) {
	source := flowacct.NewConntrack()
	instance, _ := os.Hostname()
	bootID := fmt.Sprintf("%s-%d", instance, time.Now().UnixNano())
	reader, cached := mgr.GetAPIReader(), mgr.GetClient()
	collector := flowacct.New(source, func() flowacct.Topology { return buildTopology(ctx, cached, cfg.Namespace) }, bootID, flowacct.DefaultPollEvery)
	reporter := &flowacct.Reporter{Reader: reader, Writer: cached, Namespace: cfg.Namespace, Instance: instance, Collector: collector, CounterReader: ipt, OnResume: ipt.RestoreCounterBindings}
	reporter.OnPublish = func(ctx context.Context, report flowacct.Report) error {
		return publishAccessTotals(ctx, cached, cfg.Namespace, report)
	}
	if err := reporter.Resume(ctx); err != nil {
		source.Close()
		return nil, nil, err
	}
	ipt.BeforeRetire = func(snapshot flowacct.CounterSnapshot) error {
		if err := collector.ObserveCounters(snapshot); err != nil {
			return err
		}
		return reporter.Publish(ctx, snapshot.At)
	}
	ipt.AfterRetire = collector.ForgetBindings
	return reporter, source, nil
}

func accessTotals(report flowacct.Report) map[string]vpn.TrafficCounter {
	totals := map[string]vpn.TrafficCounter{}
	for _, row := range report.Ledger {
		totals[vpn.AccessRule{ClientName: row.Subject, LabName: row.Lab, Action: vpn.AccessAllow}.Identifier()] = vpn.TrafficCounter{Packets: row.PacketsOut, Bytes: row.BytesOut}
	}
	return totals
}
func publishAccessTotals(ctx context.Context, c client.Client, namespace string, report flowacct.Report) error {
	policy := &laboratoryv1alpha1.LabGroupAccessPolicy{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: names.LabGroupAccessPolicyName}, policy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	base := policy.DeepCopy()
	totals := accessTotals(report)
	for i := range policy.Status.Rules {
		row := &policy.Status.Rules[i]
		if row.Action != laboratoryv1alpha1.LabGroupAccessAllow {
			row.Packets = 0
			row.Bytes = 0
			continue
		}
		counter := totals[vpn.AccessRule{ClientName: row.ClientName, LabName: row.LabName, Action: vpn.AccessAllow}.Identifier()]
		row.Packets = counter.Packets
		row.Bytes = counter.Bytes
		row.CounterReset = report.Partial
	}
	return c.Status().Patch(ctx, policy, client.MergeFrom(base))
}

func buildTopology(ctx context.Context, c client.Client, namespace string) flowacct.Topology {
	topo := flowacct.Topology{Clients: map[netip.Addr]string{}}
	var clients laboratoryv1alpha1.LabGroupClientList
	if c.List(ctx, &clients, client.InNamespace(namespace)) == nil {
		for i := range clients.Items {
			if addr, ok := parseAddr(clients.Items[i].Status.AssignedIP); ok {
				topo.Clients[addr] = clients.Items[i].Name
			}
		}
	}
	var legs laboratoryv1alpha1.LabVPNList
	indices := map[string]uint16{}
	if c.List(ctx, &legs, client.InNamespace(namespace)) == nil {
		for _, leg := range legs.Items {
			indices[leg.Spec.LabName] = uint16(leg.Spec.NetworkIndex)
		}
	}
	var labs laboratoryv1alpha1.LabList
	if c.List(ctx, &labs, client.InNamespace(namespace)) == nil {
		for i := range labs.Items {
			prefix, err := netip.ParsePrefix(labs.Items[i].Status.VPN.CIDR)
			if err != nil {
				continue
			}
			topo.Labs = append(topo.Labs, flowacct.LabNet{Name: labs.Items[i].Name, Prefix: prefix, Index: indices[labs.Items[i].Name]})
		}
	}
	return topo
}

// parseAddr accepts "10.8.0.5" and "10.8.0.5/32".
func parseAddr(value string) (netip.Addr, bool) {
	value, _, _ = strings.Cut(value, "/")
	addr, err := netip.ParseAddr(value)
	return addr, err == nil
}
