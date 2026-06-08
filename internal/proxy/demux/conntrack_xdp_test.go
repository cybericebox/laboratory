package demux

import (
	"net"
	"sync"
	"testing"
	"time"
)

type mockEntry struct {
	dst Socket
	src Socket
}

type mockXDP struct {
	mu      sync.Mutex
	updated map[uint32]mockEntry
	deleted []uint32
}

func newMockXDP() *mockXDP {
	return &mockXDP{updated: make(map[uint32]mockEntry)}
}

func (m *mockXDP) Update(ri uint32, dstIP net.IP, dstPort uint16, srcIP net.IP, srcPort uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated[ri] = mockEntry{
		dst: Socket{IP: dstIP, Port: dstPort},
		src: Socket{IP: srcIP, Port: srcPort},
	}
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

	xdp.mu.Lock()
	defer xdp.mu.Unlock()
	// si=11: type-4 from client (ri=si) → forward to server; expected src = client
	if got, ok := xdp.updated[11]; !ok || got.dst.Port != serverSock.Port {
		t.Errorf("si=11 dst should be serverSock port %d, got %+v ok=%v", serverSock.Port, got, ok)
	}
	if got := xdp.updated[11]; got.src.Port != clientSock.Port {
		t.Errorf("si=11 src should be clientSock port %d, got %+v", clientSock.Port, got)
	}
	// ci=22: type-4 from server (ri=ci) → forward to client; expected src = server
	if got, ok := xdp.updated[22]; !ok || got.dst.Port != clientSock.Port {
		t.Errorf("ci=22 dst should be clientSock port %d, got %+v ok=%v", clientSock.Port, got, ok)
	}
	if got := xdp.updated[22]; got.src.Port != serverSock.Port {
		t.Errorf("ci=22 src should be serverSock port %d, got %+v", serverSock.Port, got)
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

	// ci=66's XDP entry dst must point to newClient after roaming.
	xdp.mu.Lock()
	defer xdp.mu.Unlock()
	if got, ok := xdp.updated[66]; !ok || got.dst.Port != newClient.Port {
		t.Errorf("after roaming, ci=66 XDP dst should be newClient port %d, got %+v ok=%v", newClient.Port, got, ok)
	}
}

func TestXDP_LookupForward_Roaming_UpdatesBothXDPEntries(t *testing.T) {
	ct := NewConnTrack()
	xdp := newMockXDP()
	ct.SetXDP(xdp)

	clientSock := Socket{IP: net.ParseIP("10.0.0.1"), Port: 9000}
	serverSock := Socket{IP: net.ParseIP("10.1.0.1"), Port: 51820}
	// AddPartial creates entries[ci=66]; Complete fills in entries[si=55] and sets peer index.
	ct.AddPartial(66, clientSock, serverSock)
	ct.Complete(55, 66, clientSock, serverSock)

	roamedClient := Socket{IP: net.ParseIP("10.0.0.99"), Port: 9999}
	dst, found := ct.LookupForward(55, roamedClient)
	if !found {
		t.Fatal("LookupForward returned not found")
	}
	if dst.Port != serverSock.Port {
		t.Errorf("forward dst should be serverSock port %d, got %d", serverSock.Port, dst.Port)
	}

	xdp.mu.Lock()
	defer xdp.mu.Unlock()
	// si=55 entry: same dst (server), new expected src (roamed client)
	if got := xdp.updated[55]; got.dst.Port != serverSock.Port || got.src.Port != roamedClient.Port {
		t.Errorf("si=55 after roam: want dst=%d src=%d, got dst=%d src=%d",
			serverSock.Port, roamedClient.Port, got.dst.Port, got.src.Port)
	}
	// ci=66 entry: new dst (roamed client), same expected src (server)
	if got := xdp.updated[66]; got.dst.Port != roamedClient.Port || got.src.Port != serverSock.Port {
		t.Errorf("ci=66 after roam: want dst=%d src=%d, got dst=%d src=%d",
			roamedClient.Port, serverSock.Port, got.dst.Port, got.src.Port)
	}
}

func TestXDP_NilSafe(t *testing.T) {
	ct := NewConnTrack()
	// No SetXDP — xdp is nil; must not panic
	ct.Complete(1, 2, Socket{IP: net.ParseIP("1.1.1.1"), Port: 100}, Socket{IP: net.ParseIP("2.2.2.2"), Port: 200})
	ct.UpdateRoaming(1, Socket{IP: net.ParseIP("3.3.3.3"), Port: 300})
	ct.evictEntry(1)
}
