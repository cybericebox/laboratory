package laboratory

import (
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// vpnLabInitiatedDropped is the packets that the VPN gateway dropped because a VPN gateway without reverseConnections sent them towards a
// VPN client (new connections and anything that was not a reply). It is for the cluster administrator: the gateway writes the count on the
// lab's LabVPN, the operator exposes it here, and it is not part of the agent API, so the platform backend never sees it. The count restarts
// when the VPN process of the group restarts.
var vpnLabInitiatedDropped = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "laboratory_vpn_lab_initiated_dropped_packets",
	Help: "Packets from a lab towards VPN clients dropped because the lab may not start connections (since the VPN process started).",
}, []string{"namespace", "lab"})

func init() { metrics.Registry.MustRegister(vpnLabInitiatedDropped) }

// publishLabInitiatedDropped sets the metric of a lab: the sum over its VPN gateways that confirmed the block, or removes it when no
// gateway of the lab has one to report.
func publishLabInitiatedDropped(namespace, lab string, dropped int64, blocked bool) {
	if !blocked {
		vpnLabInitiatedDropped.DeleteLabelValues(namespace, lab)
		return
	}
	vpnLabInitiatedDropped.WithLabelValues(namespace, lab).Set(float64(dropped))
}

// labInitiatedBlockedStatus is what a VPN gateway of a lab reports about the block of connections its devices start towards VPN clients:
// true only for a VPN gateway whose switch is off AND whose program has confirmed it on the LabVPN. A group whose VPN pod runs an older
// program never confirms, so its labs are never reported as blocked.
func labInitiatedBlockedStatus(g laboratoryv1alpha1.GatewayTemplate, leg *laboratoryv1alpha1.LabVPN) bool {
	return g.Kind == laboratoryv1alpha1.GatewayKindVPN && !g.ReverseConnections && leg != nil && leg.Status.LabInitiatedBlocked
}
