package bandwidth

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/cybericebox/laboratory/internal/names"
)

// Settings are the chart's bandwidth values as the operator and the node-agent read them from the environment.
type Settings struct {
	// Enabled turns the whole protection on. Off: nothing is limited and nothing is written.
	Enabled            bool    `env:"BANDWIDTH_ENABLED" envDefault:"false"`
	DefaultMbit        int64   `env:"BANDWIDTH_DEFAULT_MBIT" envDefault:"1000"`
	SystemReserveMbit  int64   `env:"BANDWIDTH_SYSTEM_RESERVE_MBIT" envDefault:"100"`
	EntryShare         int64   `env:"BANDWIDTH_ENTRY_SHARE" envDefault:"50"`
	InternetShare      int64   `env:"BANDWIDTH_INTERNET_SHARE" envDefault:"25"`
	GeneveShare        int64   `env:"BANDWIDTH_GENEVE_SHARE" envDefault:"25"`
	ParticipantCapMbit int64   `env:"BANDWIDTH_PARTICIPANT_CAP_MBIT" envDefault:"50"`
	LabInternetCapMbit int64   `env:"BANDWIDTH_LAB_INTERNET_CAP_MBIT" envDefault:"50"`
	LabGeneveCapMbit   int64   `env:"BANDWIDTH_LAB_GENEVE_CAP_MBIT" envDefault:"100"`
	GeneveOverhead     float64 `env:"BANDWIDTH_GENEVE_OVERHEAD_FACTOR" envDefault:"0.85"`
	EntryOverhead      float64 `env:"BANDWIDTH_ENTRY_OVERHEAD_FACTOR" envDefault:"0.85"`
}

// Config is the Settings as a Config.
func (s Settings) Config() Config {
	return Config{
		DefaultMbit: s.DefaultMbit, SystemReserveMbit: s.SystemReserveMbit,
		EntryShare: s.EntryShare, InternetShare: s.InternetShare, GeneveShare: s.GeneveShare,
		ParticipantCapMbit: s.ParticipantCapMbit, LabInternetCapMbit: s.LabInternetCapMbit, LabGeneveCapMbit: s.LabGeneveCapMbit,
		GeneveOverhead: s.GeneveOverhead, EntryOverhead: s.EntryOverhead,
	}
}

// NodeMbit is the bandwidth a node's label says, in Mbit/s; 0 when it has none or it cannot be read.
func NodeMbit(node *corev1.Node) int64 {
	if node == nil {
		return 0
	}
	v, _ := ParseMbit(node.Labels[names.LabelNodeBandwidth])
	return v
}

// ReserveMbit is what a platform pod keeps for itself, in Mbit/s (0 when it declares nothing or the value is not a number).
func ReserveMbit(pod *corev1.Pod) int64 {
	v, _ := ParseMbit(pod.Annotations[names.AnnotationBandwidthReserve])
	return v
}

// ForNode computes a node: its label, and the reserves of the pods that run on it (pods of other nodes are ignored, and so are
// pods that are over).
func (c Config) ForNode(node *corev1.Node, pods []corev1.Pod) NodeLimits {
	var reserves []int64
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != node.Name || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if r := ReserveMbit(p); r > 0 {
			reserves = append(reserves, r)
		}
	}
	return c.Compute(NodeMbit(node), reserves)
}

// NodeGeneveLimits are the limits the node-agent applies for the Geneve traffic of a node.
func (c Config) NodeGeneveLimits(n NodeLimits) Limits {
	return Limits{V: Version, GeneveKbps: n.GeneveKbps, LabCapKbps: c.LabGeneveCapMbit * 1000}
}
