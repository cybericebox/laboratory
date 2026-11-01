// Package bandwidth is the bandwidth protection of the laboratory: how much a node can carry, how that is divided between the
// entry (demux and L7 proxy), the internet (the gateways) and the Geneve fabric, and the token buckets the pods use to keep to it.
//
// The numbers: U = the bandwidth of the node (label) - the system reserve - the reserves of the platform pods on that node. Of U the
// entry gets EntryShare percent, the internet InternetShare, Geneve GeneveShare. Inside a class everyone gets a fair share, never
// more than the cap of its kind (a participant, a lab's internet, a lab's Geneve). Nothing inside a node is limited.
//
// The operator computes the limits when something changes and writes them as annotations; the pods apply them (nothing here polls).
package bandwidth

import (
	"fmt"
	"strconv"
	"strings"
)

// Config is the chart's bandwidth section.
type Config struct {
	// DefaultMbit is the bandwidth of a node without the label.
	DefaultMbit int64
	// SystemReserveMbit is taken off every node: the kubelet, the control plane traffic, image pulls.
	SystemReserveMbit int64
	// EntryShare, InternetShare and GeneveShare are the percent of U each class gets (their sum is at most 100).
	EntryShare, InternetShare, GeneveShare int64
	// ParticipantCapMbit, LabInternetCapMbit and LabGeneveCapMbit are the most one participant, one lab's internet and one lab's
	// Geneve traffic may take, whatever is free.
	ParticipantCapMbit, LabInternetCapMbit, LabGeneveCapMbit int64
	// GeneveOverhead is the share of the Geneve class an inner frame may use: the OVS meter counts the frame before the
	// encapsulation, and the outer headers (about 50 bytes) come on top. 0.85 leaves room for packets down to a few hundred bytes.
	GeneveOverhead float64
	// EntryOverhead is the share of the entry class that payload may use on the way to a VPN pod: the VXLAN of the cluster network adds
	// about 50 bytes to every packet the demux and the L7 proxy forward to another node.
	EntryOverhead float64
}

// DefaultConfig mirrors the chart values.
func DefaultConfig() Config {
	return Config{
		DefaultMbit: 1000, SystemReserveMbit: 100,
		EntryShare: 50, InternetShare: 25, GeneveShare: 25,
		ParticipantCapMbit: 50, LabInternetCapMbit: 50, LabGeneveCapMbit: 100,
		GeneveOverhead: 0.85, EntryOverhead: 0.85,
	}
}

// Validate checks the shares and caps.
func (c Config) Validate() error {
	if c.DefaultMbit <= 0 {
		return fmt.Errorf("bandwidth: the default node bandwidth must be positive")
	}
	if c.SystemReserveMbit < 0 || c.ParticipantCapMbit < 0 || c.LabInternetCapMbit < 0 || c.LabGeneveCapMbit < 0 {
		return fmt.Errorf("bandwidth: reserves and caps cannot be negative")
	}
	if c.EntryShare < 0 || c.InternetShare < 0 || c.GeneveShare < 0 || c.EntryShare+c.InternetShare+c.GeneveShare > 100 {
		return fmt.Errorf("bandwidth: the shares must be non-negative and add up to at most 100, got %d+%d+%d", c.EntryShare, c.InternetShare, c.GeneveShare)
	}
	if c.GeneveOverhead <= 0 || c.GeneveOverhead > 1 {
		return fmt.Errorf("bandwidth: the Geneve overhead factor must be in (0, 1], got %v", c.GeneveOverhead)
	}
	if c.EntryOverhead <= 0 || c.EntryOverhead > 1 {
		return fmt.Errorf("bandwidth: the entry overhead factor must be in (0, 1], got %v", c.EntryOverhead)
	}
	return nil
}

// EntryClassKbps is the entry class one demux (or L7 proxy) may use, given the smallest entry class of the nodes it can send to and
// the number of nodes that can send. The demux of every lab node may forward to a VPN pod on one node, so each gets a 1/senders share
// of the half of the receiving node's entry class that is for the demux, scaled by the overhead factor: all of them sending to one node
// together stay within what that node was given. senders < 1 counts as one.
func (c Config) EntryClassKbps(minEntryKbps int64, senders int) int64 {
	if senders < 1 {
		senders = 1
	}
	factor := c.EntryOverhead
	if factor <= 0 || factor > 1 {
		factor = 1
	}
	return max(int64(float64(minEntryKbps)*factor)/2/int64(senders), 1)
}

// NodeLimits are the classes of one node in kbit/s, for each direction (the same number both ways).
type NodeLimits struct {
	// UKbps is the bandwidth left for labs and participants on the node.
	UKbps int64
	// EntryKbps, InternetKbps and GeneveKbps are the classes; GeneveKbps is already scaled by the overhead factor.
	EntryKbps, InternetKbps, GeneveKbps int64
}

// Compute divides a node: bandwidth is the label's value in Mbit/s (0 = the default), reserves the reserves of the platform pods
// that run on it in Mbit/s.
func (c Config) Compute(bandwidthMbit int64, reservesMbit []int64) NodeLimits {
	if bandwidthMbit <= 0 {
		bandwidthMbit = c.DefaultMbit
	}
	u := bandwidthMbit - c.SystemReserveMbit
	for _, r := range reservesMbit {
		if r > 0 {
			u -= r
		}
	}
	if u < 0 {
		u = 0
	}
	ukbps := u * 1000
	return NodeLimits{
		UKbps:        ukbps,
		EntryKbps:    ukbps * c.EntryShare / 100,
		InternetKbps: ukbps * c.InternetShare / 100,
		GeneveKbps:   int64(float64(ukbps*c.GeneveShare/100) * c.GeneveOverhead),
	}
}

// FairShare is what each of n active users of a class of classKbps gets: an equal part, never more than capKbps. A class or cap of 0
// is no limit of that kind (class 0 and cap 0 together: 0, which means unlimited to the callers).
func FairShare(classKbps, capKbps int64, active int) int64 {
	if active < 1 {
		active = 1
	}
	share := classKbps / int64(active)
	switch {
	case classKbps <= 0:
		return capKbps
	case capKbps > 0 && share > capKbps:
		return capKbps
	case share < 1:
		return 1
	}
	return share
}

// LabInternetKbps is the ceiling of one lab's internet traffic when labs labs share the internet class of the node.
func (c Config) LabInternetKbps(n NodeLimits, labs int) int64 {
	return FairShare(n.InternetKbps, c.LabInternetCapMbit*1000, labs)
}

// LabGeneveKbps is the rate of the OVS meter of one lab that is split between nodes, when splitLabs such labs are on the node and
// the lab's frames go to vteps remote nodes (a flooded frame is copied to each, the meter counts it once, so the rate is divided).
func (c Config) LabGeneveKbps(n NodeLimits, splitLabs, vteps int) int64 {
	return LabGeneveRate(n.GeneveKbps, c.LabGeneveCapMbit*1000, splitLabs, vteps)
}

// LabGeneveRate is LabGeneveKbps from the class and the cap in kbit/s, which is all the node-agent is given.
func LabGeneveRate(classKbps, labCapKbps int64, splitLabs, vteps int) int64 {
	if vteps < 1 {
		vteps = 1
	}
	return max(FairShare(classKbps, labCapKbps, splitLabs)/int64(vteps), 1)
}

// NodeGeneveKbps is the rate of the node meter, the one every split lab goes through: the class divided by the most remote nodes
// any lab floods to.
func NodeGeneveKbps(n NodeLimits, maxVTEPs int) int64 {
	if maxVTEPs < 1 {
		maxVTEPs = 1
	}
	return max(n.GeneveKbps/int64(maxVTEPs), 1)
}

// ParseMbit reads a bandwidth: a number of Mbit/s with an optional trailing M, or G for Gbit/s ("1562", "1562M", "10G").
// ok is false for an empty or unreadable value, a zero or a negative one.
func ParseMbit(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'M', 'm':
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1000
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v > 1<<40 {
		return 0, false
	}
	return v * mult, true
}
