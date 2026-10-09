//go:build linux

package flowacct

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/ti-mo/conntrack"
)

// AccountingSysctls are the conntrack switches the collector needs for byte
// counts and flow start times. Without them the flags still work; only bytes,
// packets and start times are zero.
var AccountingSysctls = []string{
	"/proc/sys/net/netfilter/nf_conntrack_acct",
	"/proc/sys/net/netfilter/nf_conntrack_timestamp",
}

// AccountingEnabled reports whether the kernel accounts bytes in this netns.
func AccountingEnabled() bool {
	raw, err := os.ReadFile(AccountingSysctls[0])
	return err == nil && strings.TrimSpace(string(raw)) == "1"
}

// Conntrack is the ctnetlink Source. The netlink socket lives in the netns of
// the calling process, which for the VPN pod is the netns of wg0 and the lab
// ports.
type Conntrack struct {
	mu   sync.Mutex
	conn *conntrack.Conn
}

func NewConntrack() *Conntrack { return &Conntrack{} }

func (c *Conntrack) Dump() ([]Flow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		conn, err := conntrack.Dial(nil)
		if err != nil {
			return nil, fmt.Errorf("dial ctnetlink: %w", err)
		}
		c.conn = conn
	}
	raw, err := c.conn.Dump(nil)
	if err != nil {
		_ = c.conn.Close()
		c.conn = nil
		return nil, fmt.Errorf("dump conntrack: %w", err)
	}
	flows := make([]Flow, 0, len(raw))
	for i := range raw {
		f := &raw[i]
		orig := f.TupleOrig
		if !orig.IP.SourceAddress.IsValid() || !orig.IP.DestinationAddress.IsValid() {
			continue
		}
		flows = append(flows, Flow{
			ID:         f.ID,
			Mark:       f.Mark,
			Proto:      protoName(orig.Proto.Protocol),
			Src:        orig.IP.SourceAddress,
			Dst:        orig.IP.DestinationAddress,
			SrcPort:    orig.Proto.SourcePort,
			DstPort:    orig.Proto.DestinationPort,
			Start:      f.Timestamp.Start,
			Replied:    f.Status.SeenReply(),
			PacketsOut: f.CountersOrig.Packets,
			BytesOut:   f.CountersOrig.Bytes,
			PacketsIn:  f.CountersReply.Packets,
			BytesIn:    f.CountersReply.Bytes,
		})
	}
	return flows, nil
}

func (c *Conntrack) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func protoName(n uint8) string {
	switch n {
	case 1:
		return "icmp"
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 58:
		return "icmpv6"
	case 132:
		return "sctp"
	default:
		return strconv.Itoa(int(n))
	}
}
