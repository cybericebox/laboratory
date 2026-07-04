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
}

type ConnTrack struct {
	mu      sync.RWMutex
	entries map[uint32]*ConnEntry
}

func NewConnTrack() *ConnTrack {
	return &ConnTrack{entries: make(map[uint32]*ConnEntry)}
}

// AddPartial creates the Ci entry after type 1 forward (Si unknown yet).
// Returns false (and changes nothing) when the index is already taken by a
// different live session — spec §4 mandates dropping the handshake so upstream
// WireGuard retries with a new index, instead of silently overwriting a peer.
func (c *ConnTrack) AddPartial(ci uint32, clientSocket, serverSocket Socket) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[ci]; ok && !isStaleLocked(existing) {
		if !sameClient(existing, clientSocket) {
			return false
		}
		// Same client retransmitting; refresh in place.
		existing.SenderSocket = clientSocket
		existing.ReceiverSocket = serverSocket
		existing.LastSeen = time.Now()
		return true
	}
	c.entries[ci] = &ConnEntry{
		SenderSocket:   clientSocket,
		ReceiverSocket: serverSocket,
		LastSeen:       time.Now(),
	}
	return true
}

// Complete fills in Si after type 2 response, creating mirror entry.
// Returns false when Si collides with a live session that is not the matching
// peer — drop the response and let the handshake fail; upstream WG retries.
func (c *ConnTrack) Complete(si, ci uint32, clientSocket, serverSocket Socket) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[si]; ok && !isStaleLocked(existing) {
		// Same backend retransmitting type 2 for an already-completed session?
		// Allow; otherwise drop to avoid clobbering an unrelated peer.
		if existing.PeerIndex != ci {
			return false
		}
	}
	c.entries[si] = &ConnEntry{
		PeerIndex:      ci,
		SenderSocket:   serverSocket,
		ReceiverSocket: clientSocket,
		LastSeen:       time.Now(),
	}
	if e, ok := c.entries[ci]; ok {
		e.PeerIndex = si
	}
	return true
}

// isStaleLocked reports whether an entry is past TTL. Must be called with c.mu held.
func isStaleLocked(e *ConnEntry) bool {
	return time.Since(e.LastSeen) > conntrackTTL
}

// sameClient reports whether the given socket matches the entry's recorded sender.
func sameClient(e *ConnEntry, s Socket) bool {
	return e.SenderSocket.Port == s.Port && e.SenderSocket.IP.Equal(s.IP)
}

// Lookup finds the entry, updates LastSeen, and returns ReceiverSocket.
// Used for type-4 userspace fallback: receiverIndex → forward to ReceiverSocket.
func (c *ConnTrack) Lookup(receiverIndex uint32) (dst Socket, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[receiverIndex]
	if !ok {
		return Socket{}, false
	}
	e.LastSeen = time.Now()
	return e.ReceiverSocket, true
}

// LookupSender returns SenderSocket for the given index without updating LastSeen.
// Used for type-2: ci entry holds SenderSocket = original WireGuard client.
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
// If the source no longer matches the expected socket we update for roaming.
func (c *ConnTrack) LookupForward(receiverIndex uint32, src Socket) (dst Socket, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[receiverIndex]
	if !ok {
		return Socket{}, false
	}
	e.LastSeen = time.Now()
	// SenderSocket is the owner of receiverIndex — the correct forward target.
	// ReceiverSocket is the expected source of packets carrying this index.
	if !e.ReceiverSocket.IP.Equal(src.IP) || e.ReceiverSocket.Port != src.Port {
		// Client (or server) has roamed — update both endpoints.
		e.ReceiverSocket = src
		if peer, peerOk := c.entries[e.PeerIndex]; peerOk {
			peer.SenderSocket = src
		}
	}
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

// evictEntry removes idx and its peer from the map.
// Must be called with c.mu held.
func (c *ConnTrack) evictEntry(idx uint32) {
	e, ok := c.entries[idx]
	if !ok {
		return
	}
	peer := e.PeerIndex
	delete(c.entries, idx)
	delete(c.entries, peer)
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
			c.mu.Lock()
			var expired []uint32
			for idx, e := range c.entries {
				if time.Since(e.LastSeen) > conntrackTTL {
					expired = append(expired, idx)
				}
			}
			for _, idx := range expired {
				c.evictEntry(idx)
			}
			c.mu.Unlock()
		}
	}
}
