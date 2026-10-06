//go:build linux

package nodeagent

import (
	"context"
	"net"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GeneveSourceSync keeps the Geneve ingress of the bridge limited to the nodes of the cluster (R-12): every Interval it reads the
// nodes' internal addresses and gives them to FlowManager.SetGeneveSources. A node that joins is accepted within one interval; one
// that leaves stops being accepted. The cluster's own address is included (harmless), and a read that fails keeps the last set.
type GeneveSourceSync struct {
	Reader   client.Reader
	Flows    *FlowManager
	Interval time.Duration
}

// NeedLeaderElection: every node-agent runs it for itself.
func (g *GeneveSourceSync) NeedLeaderElection() bool { return false }

// NodeAddresses are the internal IPv4 addresses of the nodes, sorted.
func NodeAddresses(nodes []corev1.Node) []net.IP {
	var out []net.IP
	for i := range nodes {
		for _, a := range nodes[i].Status.Addresses {
			if a.Type != corev1.NodeInternalIP {
				continue
			}
			if ip := net.ParseIP(a.Address); ip != nil && ip.To4() != nil {
				out = append(out, ip.To4())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i]) < string(out[j]) })
	return out
}

// Sync does one pass.
func (g *GeneveSourceSync) Sync(ctx context.Context) error {
	var nodes corev1.NodeList
	if err := g.Reader.List(ctx, &nodes); err != nil {
		return err
	}
	return g.Flows.SetGeneveSources(NodeAddresses(nodes.Items))
}

// Start runs until ctx ends.
func (g *GeneveSourceSync) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("geneve-sources")
	every := g.Interval
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := g.Sync(ctx); err != nil {
			log.Error(err, "sync the nodes the Geneve ingress accepts")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
