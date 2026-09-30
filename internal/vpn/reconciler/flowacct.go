//go:build linux

package reconciler

import (
	"context"
	"fmt"
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

// RunFlowAccounting counts connections from VPN clients to the lab networks of
// this group and publishes them as a LabTrafficReport. It only reads conntrack
// and the cluster cache; it never touches the firewall or WireGuard, and a
// failure here is logged and retried without affecting the data path.
func RunFlowAccounting(ctx context.Context, mgr ctrl.Manager, cfg *vpn.Config) {
	log := ctrl.Log.WithName("flowacct")
	if !flowacct.AccountingEnabled() {
		log.Info("conntrack byte accounting is off; attempts and replies are still counted, bytes stay zero")
	}
	source := flowacct.NewConntrack()
	defer source.Close()

	instance, _ := os.Hostname()
	bootID := fmt.Sprintf("%s-%d", instance, time.Now().UnixNano())
	reader := mgr.GetAPIReader()
	cached := mgr.GetClient()
	topology := func() flowacct.Topology { return buildTopology(ctx, cached, cfg.Namespace) }

	reporter := &flowacct.Reporter{
		Reader: reader, Writer: cached, Namespace: cfg.Namespace, Instance: instance,
		Collector: flowacct.New(source, topology, bootID, flowacct.DefaultPollEvery),
	}
	reporter.Run(ctx, flowacct.DefaultPollEvery, flowacct.DefaultReportEvery, func(err error) {
		log.Error(err, "flow accounting")
	})
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
	var labs laboratoryv1alpha1.LabList
	if c.List(ctx, &labs, client.InNamespace(namespace)) == nil {
		for i := range labs.Items {
			prefix, err := netip.ParsePrefix(labs.Items[i].Status.VPN.CIDR)
			if err != nil {
				continue
			}
			topo.Labs = append(topo.Labs, flowacct.LabNet{Name: labs.Items[i].Name, Prefix: prefix})
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
