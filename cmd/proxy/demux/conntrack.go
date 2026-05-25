package demux

import (
	"net"
	"sync"
	"time"
)

const conntrackTTL = 3 * time.Minute

// XDPSessions is implemented by xdp.XDPHandle on linux and a nil-safe no-op otherwise.
type XDPSessions interface {
	Update(receiverIndex uint32, ip net.IP, port uint16) error
	Delete(receiverIndex uint32) error
}

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
	xdp     XDPSessions
}

func NewConnTrack() *ConnTrack {
	return &ConnTrack{entries: make(map[uint32]*ConnEntry)}
}

// SetXDP injects the XDP handle for BPF map synchronisation. Call once after xdp.Load().
func (c *ConnTrack) SetXDP(x XDPSessions) { c.xdp = x }

// AddPartial creates the Ci entry after type 1 forward (Si unknown yet).
func (c *ConnTrack) AddPartial(ci uint32, clientSocket, serverSocket Socket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[ci] = &ConnEntry{
		SenderSocket:   clientSocket,
		ReceiverSocket: serverSocket,
		LastSeen:       time.Now(),
	}
}

// Complete fills in Si after type 2 response, creating mirror entry.
// XDP map: ci → serverSocket (client→server), si → clientSocket (server→client).
func (c *ConnTrack) Complete(si, ci uint32, clientSocket, serverSocket Socket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[si] = &ConnEntry{
		PeerIndex:      ci,
		SenderSocket:   serverSocket,
		ReceiverSocket: clientSocket,
		LastSeen:       time.Now(),
	}
	if e, ok := c.entries[ci]; ok {
		e.PeerIndex = si
	}
	if c.xdp != nil {
		_ = c.xdp.Update(ci, serverSocket.IP, serverSocket.Port)
		_ = c.xdp.Update(si, clientSocket.IP, clientSocket.Port)
	}
}

// Lookup finds the entry and updates LastSeen.
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
	if c.xdp != nil && e.PeerIndex != 0 {
		_ = c.xdp.Update(e.PeerIndex, newSender.IP, newSender.Port)
	}
}

// evictEntry removes idx and its peer from the map, notifying XDP.
// Must be called with c.mu held.
func (c *ConnTrack) evictEntry(idx uint32) {
	e, ok := c.entries[idx]
	if !ok {
		return
	}
	peer := e.PeerIndex
	delete(c.entries, idx)
	delete(c.entries, peer)
	if c.xdp != nil {
		_ = c.xdp.Delete(idx)
		_ = c.xdp.Delete(peer)
	}
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
