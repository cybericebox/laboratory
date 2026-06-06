package demux

import (
	"net"
	"sync"
	"testing"
	"time"
)

type mockXDP struct {
	mu      sync.Mutex
	updated map[uint32]Socket
	deleted []uint32
}

func newMockXDP() *mockXDP {
	return &mockXDP{updated: make(map[uint32]Socket)}
}

func (m *mockXDP) Update(ri uint32, ip net.IP, port uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated[ri] = Socket{IP: ip, Port: port}
	return nil
}

func (m *mockXDP) Delete(ri uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, ri)
	return nil
}

func contains(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestXDP_Complete_UpdatesBothDirections(t *testing.T) {
	ct := NewConnTrack()
	xdp := newMockXDP()
	ct.SetXDP(xdp)

	clientSock := Socket{IP: net.ParseIP("10.0.0.1"), Port: 12345}
	serverSock := Socket{IP: net.ParseIP("10.1.0.1"), Port: 51820}

	ct.Complete(11, 22, clientSock, serverSock)

	// ci=22 → serverSocket (client→server direction)
	xdp.mu.Lock()
	defer xdp.mu.Unlock()
	if got, ok := xdp.updated[22]; !ok || got.Port != serverSock.Port {
		t.Errorf("ci=22 should map to serverSock port %d, got %+v ok=%v", serverSock.Port, got, ok)
	}
	// si=11 → clientSocket (server→client direction)
	if got, ok := xdp.updated[11]; !ok || got.Port != clientSock.Port {
		t.Errorf("si=11 should map to clientSock port %d, got %+v ok=%v", clientSock.Port, got, ok)
	}
}

func TestXDP_TTLCleanup_DeletesBothEntries(t *testing.T) {
	ct := NewConnTrack()
	xdp := newMockXDP()
	ct.SetXDP(xdp)

	clientSock := Socket{IP: net.ParseIP("10.0.0.1"), Port: 11111}
	serverSock := Socket{IP: net.ParseIP("10.1.0.1"), Port: 51820}
	ct.Complete(33, 44, clientSock, serverSock)

	// Backdate both entries past TTL.
	ct.mu.Lock()
	for _, e := range ct.entries {
		e.LastSeen = time.Now().Add(-(conntrackTTL + time.Second))
	}
	ct.mu.Unlock()

	// Reset delete tracking.
	xdp.mu.Lock()
	xdp.deleted = nil
	xdp.mu.Unlock()

	// Exercise the collect-then-evict path directly (mirrors RunTTLCleanup's inner logic).
	ct.mu.Lock()
	var expired []uint32
	for idx, e := range ct.entries {
		if time.Since(e.LastSeen) > conntrackTTL {
			expired = append(expired, idx)
		}
	}
	for _, idx := range expired {
		ct.evictEntry(idx)
	}
	ct.mu.Unlock()

	xdp.mu.Lock()
	defer xdp.mu.Unlock()
	if !contains(xdp.deleted, 33) {
		t.Error("expected si=33 deleted from XDP map")
	}
	if !contains(xdp.deleted, 44) {
		t.Error("expected ci=44 deleted from XDP map")
	}
}

func TestXDP_UpdateRoaming_UpdatesPeerEntry(t *testing.T) {
	ct := NewConnTrack()
	xdp := newMockXDP()
	ct.SetXDP(xdp)

	clientSock := Socket{IP: net.ParseIP("10.0.0.1"), Port: 9000}
	serverSock := Socket{IP: net.ParseIP("10.1.0.1"), Port: 51820}
	ct.Complete(55, 66, clientSock, serverSock)

	newClient := Socket{IP: net.ParseIP("10.0.0.2"), Port: 9001}
	ct.UpdateRoaming(55, newClient)

	// When server's ri=55 gets roaming update, ci=66's XDP entry must point to newClient.
	xdp.mu.Lock()
	defer xdp.mu.Unlock()
	if got, ok := xdp.updated[66]; !ok || got.Port != newClient.Port {
		t.Errorf("after roaming, ci=66 XDP entry should be newClient port %d, got %+v ok=%v", newClient.Port, got, ok)
	}
}

func TestXDP_NilSafe(t *testing.T) {
	ct := NewConnTrack()
	// No SetXDP — xdp is nil; must not panic
	ct.Complete(1, 2, Socket{IP: net.ParseIP("1.1.1.1"), Port: 100}, Socket{IP: net.ParseIP("2.2.2.2"), Port: 200})
	ct.UpdateRoaming(1, Socket{IP: net.ParseIP("3.3.3.3"), Port: 300})
	ct.evictEntry(1)
}
