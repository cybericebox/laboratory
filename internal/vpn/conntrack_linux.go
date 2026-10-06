//go:build linux

package vpn

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/ti-mo/conntrack"
)

// ConntrackRevoker removes tracked connections the access rules no longer allow. The
// netlink socket lives in the netns of the calling process: for the VPN pod, the netns of
// wg0 and the lab ports, which is where FORWARD runs.
type ConntrackRevoker struct {
	mu   sync.Mutex
	conn *conntrack.Conn
}

func NewConntrackRevoker() *ConntrackRevoker { return &ConntrackRevoker{} }

// Revoke deletes every connection RevokedFlows names and returns how many. A failure to
// delete one entry (it ended on its own meanwhile is not a failure) is returned after the
// rest were tried, so the reconcile that called it runs again.
func (c *ConntrackRevoker) Revoke(labCIDRs []string, rules []AccessRule, clientCIDRs ...[]string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		conn, err := conntrack.Dial(nil)
		if err != nil {
			return 0, fmt.Errorf("dial ctnetlink: %w", err)
		}
		c.conn = conn
	}
	raw, err := c.conn.Dump(nil)
	if err != nil {
		_ = c.conn.Close()
		c.conn = nil
		return 0, fmt.Errorf("dump conntrack: %w", err)
	}
	flows := make([]ConnFlow, 0, len(raw))
	for i := range raw {
		orig := raw[i].TupleOrig
		if !orig.IP.SourceAddress.IsValid() || !orig.IP.DestinationAddress.IsValid() {
			continue
		}
		flows = append(flows, ConnFlow{ID: raw[i].ID, Key: i, Src: orig.IP.SourceAddress, Dst: orig.IP.DestinationAddress})
	}
	deleted := 0
	var firstErr error
	for _, f := range RevokedFlows(flows, labCIDRs, rules, clientCIDRs...) {
		if err := c.conn.Delete(raw[f.Key]); err != nil {
			// An entry that expired between the dump and the delete is gone, which is what we want.
			if isNoEntry(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("delete conntrack entry %s -> %s: %w", f.Src, f.Dst, err)
			}
			continue
		}
		deleted++
	}
	return deleted, firstErr
}

func (c *ConntrackRevoker) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// isNoEntry: ENOENT from the kernel (the entry is already gone).
func isNoEntry(err error) bool { return errors.Is(err, syscall.ENOENT) }
