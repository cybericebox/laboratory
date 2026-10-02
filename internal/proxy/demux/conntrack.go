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

type ConnEntry struct {
	PeerIndex      uint32
	SenderSocket   Socket
	ReceiverSocket Socket
	LastSeen       time.Time
	// owner is the client address the session was opened from: the entry counts against its cap, whatever roaming does.
	owner string
	// lastRoam is when the session last changed address.
	lastRoam time.Time
	// pkts is the token bucket of the packets this session may send per second (Limits.SessionRate).
	pkts bucket
}

type ConnTrack struct {
	mu      sync.RWMutex
	entries map[uint32]*ConnEntry
	perSrc  map[string]int
	limits  Limits
	now     func() time.Time
	// owners are the packet buckets of the client addresses (Limits.OwnerRate): all the sessions of one address share one.
	owners map[string]*bucket
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
	return &ConnTrack{entries: make(map[uint32]*ConnEntry), perSrc: map[string]int{}, owners: map[string]*bucket{}, limits: l, now: time.Now}
}

// Len is the number of entries.
func (c *ConnTrack) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// room says whether two more entries (a session) fit under the global cap and the owner's. Must be called with c.mu held.
func (c *ConnTrack) roomLocked(owner string) bool {
	if c.limits.MaxEntries > 0 && len(c.entries)+2 > c.limits.MaxEntries {
		return false
	}
	if c.limits.MaxEntriesPerSource > 0 && c.perSrc[owner]+2 > c.limits.MaxEntriesPerSource {
		return false
	}
	return true
}

// put stores an entry and counts it against its owner. Must be called with c.mu held.
func (c *ConnTrack) putLocked(idx uint32, e *ConnEntry) {
	if old, ok := c.entries[idx]; ok {
		c.perSrc[old.owner]--
	}
	c.entries[idx] = e
	c.perSrc[e.owner]++
}

// removeLocked deletes an entry and its count. Must be called with c.mu held.
func (c *ConnTrack) removeLocked(idx uint32) {
	if e, ok := c.entries[idx]; ok {
		if c.perSrc[e.owner]--; c.perSrc[e.owner] <= 0 {
			delete(c.perSrc, e.owner)
		}
		delete(c.entries, idx)
	}
}

// AddPartial creates the Ci entry after type 1 forward (Si unknown yet).
// Returns false (and changes nothing) when the index is already taken by a different live session (spec §4: the
// handshake is dropped so upstream WireGuard retries with a new index, instead of overwriting a peer), or when the
// table, or the client's share of it, is full.
func (c *ConnTrack) AddPartial(ci uint32, clientSocket, serverSocket Socket) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if existing, ok := c.entries[ci]; ok && !c.staleLocked(existing) {
		if !sameClient(existing, clientSocket) {
			return false
		}
		// Same client retransmitting; refresh in place.
		existing.SenderSocket = clientSocket
		existing.ReceiverSocket = serverSocket
		existing.LastSeen = now
		return true
	}
	owner := clientSocket.IP.String()
	if !c.roomLocked(owner) {
		return false
	}
	c.putLocked(ci, &ConnEntry{SenderSocket: clientSocket, ReceiverSocket: serverSocket, LastSeen: now, owner: owner, lastRoam: now})
	return true
}

// Complete fills in Si after type 2 response, creating mirror entry.
// Returns false when Si collides with a live session that is not the matching peer, or the table is full: the response
// is dropped and the handshake fails; upstream WG retries.
func (c *ConnTrack) Complete(si, ci uint32, clientSocket, serverSocket Socket) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if existing, ok := c.entries[si]; ok && !c.staleLocked(existing) {
		// Same backend retransmitting type 2 for an already-completed session?
		// Allow; otherwise drop to avoid clobbering an unrelated peer.
		if existing.PeerIndex != ci {
			return false
		}
	}
	owner := clientSocket.IP.String()
	if _, replacing := c.entries[si]; !replacing && c.limits.MaxEntries > 0 && len(c.entries)+1 > c.limits.MaxEntries {
		return false
	}
	c.putLocked(si, &ConnEntry{PeerIndex: ci, SenderSocket: serverSocket, ReceiverSocket: clientSocket, LastSeen: now, owner: owner, lastRoam: now})
	if e, ok := c.entries[ci]; ok {
		e.PeerIndex = si
	}
	return true
}

// staleLocked reports whether an entry is past TTL. Must be called with c.mu held.
func (c *ConnTrack) staleLocked(e *ConnEntry) bool {
	return c.now().Sub(e.LastSeen) > conntrackTTL
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
	if !ok {
		return Socket{}, false
	}
	e.LastSeen = c.now()
	return e.ReceiverSocket, true
}

// LookupPartial returns the sockets of a handshake in progress: the client that sent the init (SenderSocket) and the
// backend it was forwarded to (ReceiverSocket, the only address its type-2 answer may come from).
func (c *ConnTrack) LookupPartial(index uint32) (client, backend Socket, found bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[index]
	if !ok {
		return Socket{}, Socket{}, false
	}
	return e.SenderSocket, e.ReceiverSocket, true
}

// LookupSender returns SenderSocket for the given index without updating LastSeen.
func (c *ConnTrack) LookupSender(index uint32) (Socket, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[index]
	if !ok {
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
	if !ok {
		return Socket{}, false
	}
	now := c.now()
	if !sameSocket(e.ReceiverSocket, src) {
		if now.Sub(e.lastRoam) < c.limits.RoamInterval {
			return Socket{}, false
		}
		// Client (or server) has roamed — update both endpoints.
		e.ReceiverSocket = src
		e.lastRoam = now
		if peer, peerOk := c.entries[e.PeerIndex]; peerOk {
			peer.SenderSocket = src
		}
	}
	// The demux is shared by everyone: a session, and all the sessions of one address, have a packet rate of their own.
	if !e.pkts.take(now, c.limits.SessionRate, c.limits.SessionBurst) {
		return Socket{}, false
	}
	if c.limits.OwnerRate > 0 {
		ob := c.owners[e.owner]
		if ob == nil {
			ob = &bucket{}
			c.owners[e.owner] = ob
		}
		if !ob.take(now, c.limits.OwnerRate, c.limits.OwnerBurst) {
			return Socket{}, false
		}
	}
	e.LastSeen = now
	return e.SenderSocket, true
}

// Expected is the source the session of an index expects its packets from.
func (c *ConnTrack) Expected(index uint32) (Socket, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[index]
	if !ok {
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
	if !ok || !sameSocket(e.ReceiverSocket, src) {
		return Socket{}, false
	}
	e.LastSeen = c.now()
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
	if peer, ok := c.entries[e.PeerIndex]; ok {
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
	c.removeLocked(peer)
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
	for o := range c.owners {
		if c.perSrc[o] == 0 {
			delete(c.owners, o)
		}
	}
	return len(expired)
}

// RunTTLCleanup removes stale entries in a background goroutine.
func (c *ConnTrack) RunTTLCleanup(stop <-chan struct{}) {
	ticker := time.NewTicker(conntrackTTL / 3)
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
