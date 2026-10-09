package demux

import (
	"net"
	"sync"
	"time"
)

const conntrackTTL = 3 * time.Minute

type Socket struct {
	IP   net.IP
	Port uint16
}

type sessionState struct {
	seen, roam time.Time
	packets    bucket
	group      string
	partial    bool
}

type ConnEntry struct {
	state          *sessionState
	clientIndex    bool
	PeerIndex      uint32
	SenderSocket   Socket
	ReceiverSocket Socket
	LastSeen       time.Time
	// partial is a handshake in progress: the init was forwarded and no answer has completed it (Limits.PartialTTL).
	partial bool
}

type ConnTrack struct {
	mu      sync.RWMutex
	entries map[uint32]*ConnEntry
	limits  Limits
	now     func() time.Time
}

// take refills a bucket and takes one token from it; false when it is empty. rate 0 = unlimited.
func (b *bucket) take(now time.Time, rate float64, burst int) bool {
	if rate <= 0 {
		return true
	}
	if b.at.IsZero() {
		b.tokens, b.at = float64(burst), now
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func NewConnTrack() *ConnTrack { return NewConnTrackWithLimits(DefaultLimits()) }

func NewConnTrackWithLimits(l Limits) *ConnTrack {
	return &ConnTrack{entries: make(map[uint32]*ConnEntry), limits: l, now: time.Now}
}

// Len is the number of entries.
func (c *ConnTrack) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// roomLocked says whether two more entries (a session) fit under the global cap. Must be called with c.mu held.
func (c *ConnTrack) roomLocked() bool {
	return c.limits.MaxEntries <= 0 || len(c.entries)+2 <= c.limits.MaxEntries
}

// removeLocked deletes an entry. Must be called with c.mu held.
func (c *ConnTrack) removeLocked(idx uint32) { delete(c.entries, idx) }

// AddPartial creates the Ci entry after type 1 forward (Si unknown yet).
// Returns false (and changes nothing) when the index is already taken by a different live session (spec §4: the
// handshake is dropped so upstream WireGuard retries with a new index, instead of overwriting a peer), or when the
// table is full.
func (c *ConnTrack) AddPartial(ci uint32, clientSocket, serverSocket Socket, groups ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	group := ""
	if len(groups) > 0 {
		group = groups[0]
	}
	if existing, ok := c.entries[ci]; ok && !c.staleLocked(existing) {
		if !existing.clientIndex || !sameClient(existing, clientSocket) || !sameSocket(existing.ReceiverSocket, serverSocket) || existing.state.group != group {
			return false
		}
		// Same client retransmitting; refresh in place.
		existing.SenderSocket = clientSocket
		existing.ReceiverSocket = serverSocket
		c.touchLocked(existing, now)
		return true
	}
	if _, ok := c.entries[ci]; ok {
		c.evictLocked(ci)
	}
	if !c.roomLocked() {
		return false
	}
	c.entries[ci] = &ConnEntry{SenderSocket: clientSocket, ReceiverSocket: serverSocket, LastSeen: now, partial: true, clientIndex: true, state: &sessionState{seen: now, roam: now, group: group, partial: true}}
	return true
}

// Complete fills in Si after type 2 response, creating mirror entry.
// Returns false when Si collides with a live session that is not the matching peer, or the table is full: the response
// is dropped and the handshake fails; upstream WG retries.
func (c *ConnTrack) Complete(si, ci uint32, clientSocket, serverSocket Socket) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[ci]
	if !ok || si == ci || !e.clientIndex || c.staleLocked(e) || !sameSocket(e.SenderSocket, clientSocket) || !sameSocket(e.ReceiverSocket, serverSocket) {
		return false
	}
	if existing, ok := c.entries[si]; ok {
		if !c.staleLocked(existing) {
			if existing.state != e.state || existing.PeerIndex != ci {
				return false
			}
		} else {
			c.evictLocked(si)
		}
	}
	if _, ok := c.entries[si]; !ok && c.limits.MaxEntries > 0 && len(c.entries)+1 > c.limits.MaxEntries {
		return false
	}
	if !e.partial && e.PeerIndex != si {
		return false
	}
	now := c.now()
	e.PeerIndex = si
	e.partial = false
	e.state.partial = false
	c.touchLocked(e, now)
	c.entries[si] = &ConnEntry{PeerIndex: ci, SenderSocket: serverSocket, ReceiverSocket: clientSocket, LastSeen: now, state: e.state}
	return true
}

func (c *ConnTrack) touchLocked(e *ConnEntry, now time.Time) {
	e.LastSeen = now
	e.state.seen = now
	if peer, ok := c.entries[e.PeerIndex]; ok && peer.state == e.state {
		peer.LastSeen = now
	}
}

// staleLocked reports whether an entry is past its TTL: PartialTTL for a handshake in progress, conntrackTTL for a session.
// Must be called with c.mu held.
func (c *ConnTrack) staleLocked(e *ConnEntry) bool {
	ttl := conntrackTTL
	if e.state.partial {
		ttl = c.partialTTL()
	}
	return c.now().Sub(e.state.seen) > ttl
}

func (c *ConnTrack) partialTTL() time.Duration {
	if c.limits.PartialTTL > 0 {
		return c.limits.PartialTTL
	}
	return DefaultLimits().PartialTTL
}

// sameClient reports whether the given socket matches the entry's recorded sender.
func sameClient(e *ConnEntry, s Socket) bool {
	return e.SenderSocket.Port == s.Port && e.SenderSocket.IP.Equal(s.IP)
}

func sameSocket(a, b Socket) bool { return a.Port == b.Port && a.IP.Equal(b.IP) }

// Lookup finds the entry, updates LastSeen, and returns ReceiverSocket.
// Used for type-4 userspace fallback: receiverIndex → forward to ReceiverSocket.
func (c *ConnTrack) Lookup(receiverIndex uint32) (dst Socket, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[receiverIndex]
	if !ok || c.staleLocked(e) {
		return Socket{}, false
	}
	c.touchLocked(e, c.now())
	return e.ReceiverSocket, true
}

// LookupPartial returns the sockets of a handshake in progress: the client that sent the init (SenderSocket) and the
// backend it was forwarded to (ReceiverSocket, the only address its type-2 answer may come from).
func (c *ConnTrack) LookupPartial(index uint32) (client, backend Socket, found bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[index]
	if !ok || c.staleLocked(e) {
		return Socket{}, Socket{}, false
	}
	return e.SenderSocket, e.ReceiverSocket, true
}

// LookupSender returns SenderSocket for the given index without updating LastSeen.
func (c *ConnTrack) LookupSender(index uint32) (Socket, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[index]
	if !ok || c.staleLocked(e) {
		return Socket{}, false
	}
	return e.SenderSocket, true
}

// LookupForward returns the forwarding destination for a type-4 transport packet.
// receiver_index identifies the session:
//   - entries[Si].SenderSocket = serverSocket  → forward client→server traffic
//   - entries[Ci].SenderSocket = clientSocket  → forward server→client traffic
//
// ReceiverSocket is the expected source of packets carrying this index. A packet from another source is the session
// roaming, which is allowed, but at most once per RoamInterval for a session: a packet from a third address within the
// interval is not forwarded and moves nothing (so a spoofed packet cannot keep dragging the session around).
func (c *ConnTrack) LookupForward(receiverIndex uint32, src Socket) (dst Socket, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[receiverIndex]
	if !ok || c.staleLocked(e) {
		return Socket{}, false
	}
	now := c.now()
	if !sameSocket(e.ReceiverSocket, src) {
		if e.clientIndex || now.Sub(e.state.roam) < c.limits.RoamInterval {
			return Socket{}, false
		}
		// Client (or server) has roamed — update both endpoints.
		e.ReceiverSocket = src
		e.state.roam = now
		if peer, peerOk := c.entries[e.PeerIndex]; peerOk && peer.state == e.state {
			peer.SenderSocket = src
		}
	}
	// The demux is shared by everyone: a session has a packet rate of its own.
	if !e.state.packets.take(now, c.limits.SessionRate, c.limits.SessionBurst) {
		return Socket{}, false
	}
	c.touchLocked(e, now)
	return e.SenderSocket, true
}

// Expected is the source the session of an index expects its packets from.
func (c *ConnTrack) Expected(index uint32) (Socket, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[index]
	if !ok || c.staleLocked(e) {
		return Socket{}, false
	}
	return e.ReceiverSocket, true
}

// LookupStrict is LookupForward without roaming: the packet is forwarded only from the exact source the session
// expects (cookie replies).
func (c *ConnTrack) LookupStrict(receiverIndex uint32, src Socket) (dst Socket, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[receiverIndex]
	if !ok || c.staleLocked(e) || !sameSocket(e.ReceiverSocket, src) {
		return Socket{}, false
	}
	c.touchLocked(e, c.now())
	return e.SenderSocket, true
}

// UpdateRoaming updates sender socket when client IP changes.
func (c *ConnTrack) UpdateRoaming(receiverIndex uint32, newSender Socket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[receiverIndex]
	if !ok {
		return
	}
	e.SenderSocket = newSender
	if peer, ok := c.entries[e.PeerIndex]; ok && peer.state == e.state {
		peer.ReceiverSocket = newSender
	}
}

// evictLocked removes idx and its peer from the map. Must be called with c.mu held.
func (c *ConnTrack) evictLocked(idx uint32) {
	e, ok := c.entries[idx]
	if !ok {
		return
	}
	peer := e.PeerIndex
	c.removeLocked(idx)
	if other, ok := c.entries[peer]; !e.partial && ok && other.state == e.state && other.PeerIndex == idx {
		c.removeLocked(peer)
	}
}

// Cleanup removes the stale entries now and says how many it removed.
func (c *ConnTrack) Cleanup() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expired []uint32
	for idx, e := range c.entries {
		if c.staleLocked(e) {
			expired = append(expired, idx)
		}
	}
	for _, idx := range expired {
		c.evictLocked(idx)
	}
	return len(expired)
}

// RunTTLCleanup removes stale entries in a background goroutine.
func (c *ConnTrack) RunTTLCleanup(stop <-chan struct{}) {
	// Often enough for the short life of a handshake in progress too (it must not outlive PartialTTL by minutes).
	interval := min(conntrackTTL/3, c.partialTTL()/2)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.Cleanup()
		}
	}
}

// RemoveGroup retires only sessions belonging to the changed/deleted group.
func (c *ConnTrack) RemoveGroup(group string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index, e := range c.entries {
		if e.state.group == group {
			c.evictLocked(index)
		}
	}
}
