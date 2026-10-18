package demux

import (
	"slices"
	"time"
)

// Mode is how the demux moves transport packets: in userspace, or in the kernel through a TC or XDP program.
type Mode string

const (
	ModeUserspace Mode = "userspace"
	ModeTC        Mode = "tc"
	ModeXDP       Mode = "xdp"
)

// SessionTTL is how long a completed session lives after its last packet; the fast path leaves it to the slow path after that too.
const SessionTTL = conntrackTTL

// Forward is one direction of a session as the fast path needs it.
type Forward struct {
	Index     uint32 // receiver index carried by the packets
	Session   uint32 // the client -> server direction's index in both directions: that entry owns the session's bucket and last-seen
	Expected  Socket // the only source accepted (else the packet goes to the slow path)
	NewSource Socket // source written into the packet (entry addr towards the client; probe source towards the server)
	Dest      Socket // destination written into the packet
	ToClient  bool
	// Towards the client only: Via is the address the server sends to (the client -> server direction's NewSource), and
	// ReplyIfindex the device the server's answer arrived on (0 = unknown, the replies stay on the slow path).
	Via          Socket
	ReplyIfindex int
}

// Sink mirrors the sessions of a ConnTrack into a data plane. ConnTrack calls it with its lock held, so an
// implementation must be quick and must not call back into the ConnTrack.
type Sink interface {
	Upsert(f Forward) error
	Delete(index uint32) error
}

// SourceFor gives NewSource for each direction; nil in userspace mode.
type SourceFor interface {
	TowardsServer(server Socket) (Socket, bool)
	TowardsClient(client Socket) (Socket, bool) // the entry address the client's handshake arrived at
}

// SetSink sets the data plane the sessions are mirrored into. A nil sink keeps the pure userspace behaviour.
func (c *ConnTrack) SetSink(s Sink, src SourceFor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sink, c.src = s, src
}

// SinkErrors is how many sink calls (and source lookups) failed; the slow path keeps forwarding for those sessions.
func (c *ConnTrack) SinkErrors() uint64 { return c.sinkErrors.Load() }

// forwardsLocked builds the two directions of a complete session. ok is false when a source address is unknown.
// The reply direction needs the source towards the server too (its Via), so it is never ok without it.
func (c *ConnTrack) forwardsLocked(ci, si uint32, client, server Socket, st *sessionState) (toServer, toClient Forward, okServer, okClient bool) {
	toServer = Forward{Index: si, Session: si, Expected: client, Dest: server}
	toClient = Forward{Index: ci, Session: si, Expected: server, Dest: client, ToClient: true, ReplyIfindex: st.replyIfindex}
	if c.src == nil {
		return toServer, toClient, false, false
	}
	toServer.NewSource, okServer = c.src.TowardsServer(server)
	toClient.NewSource, okClient = c.src.TowardsClient(client)
	toClient.Via = toServer.NewSource
	return toServer, toClient, okServer, okClient && okServer
}

// pushLocked mirrors both directions of the complete session of an entry. Must be called with c.mu held.
func (c *ConnTrack) pushLocked(idx uint32, e *ConnEntry) {
	if c.sink == nil || e.partial {
		return
	}
	peer, ok := c.entries[e.PeerIndex]
	if !ok || peer.state != e.state {
		return
	}
	ci, si, ce := idx, e.PeerIndex, e
	if !e.clientIndex {
		ci, si, ce = e.PeerIndex, idx, peer
	}
	toServer, toClient, okServer, okClient := c.forwardsLocked(ci, si, ce.SenderSocket, ce.ReceiverSocket, ce.state)
	c.upsertLocked(toServer, okServer)
	c.upsertLocked(toClient, okClient)
}

func (c *ConnTrack) upsertLocked(f Forward, haveSource bool) {
	if !haveSource {
		c.sinkErrors.Add(1)
		return
	}
	if err := c.sink.Upsert(f); err != nil {
		c.sinkErrors.Add(1)
	}
}

func (c *ConnTrack) deleteLocked(idx uint32) {
	if err := c.sink.Delete(idx); err != nil {
		c.sinkErrors.Add(1)
	}
}

// Seen applies the data plane's last-seen times (newer only) before a cleanup.
func (c *ConnTrack) Seen(at map[uint32]time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for idx, t := range at {
		if t.After(now) {
			t = now
		}
		e, ok := c.entries[idx]
		if !ok || e.partial || !t.After(e.state.seen) {
			continue
		}
		e.state.seen = t
		e.LastSeen = t
		if peer, ok := c.entries[e.PeerIndex]; ok && peer.state == e.state {
			peer.LastSeen = t
		}
	}
}

// Sessions lists the completed sessions as the two directions each, ordered by index, for restore checks and metrics.
func (c *ConnTrack) Sessions() []Forward {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Forward
	for ci, e := range c.entries {
		if !e.clientIndex || e.partial || c.staleLocked(e) {
			continue
		}
		si := e.PeerIndex
		if peer, ok := c.entries[si]; !ok || peer.state != e.state {
			continue
		}
		toServer, toClient, _, _ := c.forwardsLocked(ci, si, e.SenderSocket, e.ReceiverSocket, e.state)
		out = append(out, toServer, toClient)
	}
	slices.SortFunc(out, func(a, b Forward) int {
		switch {
		case a.Index < b.Index:
			return -1
		case a.Index > b.Index:
			return 1
		}
		return 0
	})
	return out
}

// SessionCount is the number of completed live sessions.
func (c *ConnTrack) SessionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, e := range c.entries {
		if e.clientIndex && !e.partial && !c.staleLocked(e) {
			n++
		}
	}
	return n
}

// Restore re-creates a completed session from the data plane after a restart (both indices, group and reply device given). The data
// plane already holds the session, so nothing is pushed back. It refuses an index that a live different session owns,
// a full table, and a session that is already past its TTL.
func (c *ConnTrack) Restore(clientIdx, serverIdx uint32, client, server Socket, group string, seen time.Time, replyIfindex int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if clientIdx == serverIdx || c.now().Sub(seen) > conntrackTTL {
		return false
	}
	// Check first, change after: a refusal leaves everything as it was. The stale owners of the two indices go with
	// their peers; the sink is told only about the peers, because the data plane holds the very indices restored here.
	victims := map[uint32]bool{}
	for _, idx := range []uint32{clientIdx, serverIdx} {
		existing, ok := c.entries[idx]
		if !ok {
			continue
		}
		if !c.staleLocked(existing) {
			return false
		}
		victims[idx] = true
		if other, ok := c.entries[existing.PeerIndex]; !existing.partial && ok && other.state == existing.state && other.PeerIndex == idx {
			victims[existing.PeerIndex] = true
		}
	}
	if c.limits.MaxEntries > 0 && len(c.entries)-len(victims)+2 > c.limits.MaxEntries {
		return false
	}
	for idx := range victims {
		e := c.entries[idx]
		c.removeLocked(idx)
		if c.sink != nil && !e.partial && idx != clientIdx && idx != serverIdx {
			c.deleteLocked(idx)
		}
	}
	st := &sessionState{seen: seen, roam: seen, group: group, replyIfindex: replyIfindex}
	c.entries[clientIdx] = &ConnEntry{state: st, clientIndex: true, PeerIndex: serverIdx, SenderSocket: client, ReceiverSocket: server, LastSeen: seen}
	c.entries[serverIdx] = &ConnEntry{state: st, PeerIndex: clientIdx, SenderSocket: server, ReceiverSocket: client, LastSeen: seen}
	return true
}
