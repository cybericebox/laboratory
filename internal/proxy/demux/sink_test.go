package demux

import (
	"errors"
	"testing"
	"time"
)

type fakeSink struct {
	upserts []Forward
	deletes []uint32
	err     error
}

func (s *fakeSink) Upsert(f Forward) error { s.upserts = append(s.upserts, f); return s.err }
func (s *fakeSink) Delete(i uint32) error  { s.deletes = append(s.deletes, i); return s.err }

type fakeSource struct{ ok bool }

var (
	probeSrc = sock("10.9.0.1", 40001)
	entrySrc = sock("203.0.113.10", 51820)
)

func (f fakeSource) TowardsServer(Socket) (Socket, bool) { return probeSrc, f.ok }
func (f fakeSource) TowardsClient(Socket) (Socket, bool) { return entrySrc, f.ok }

func sinkTrack(t *testing.T) (*ConnTrack, *time.Time, *fakeSink) {
	t.Helper()
	c, now := clockTrack(DefaultLimits())
	s := &fakeSink{}
	c.SetSink(s, fakeSource{ok: true})
	return c, now, s
}

func lastByIndex(s *fakeSink) map[uint32]Forward {
	m := map[uint32]Forward{}
	for _, f := range s.upserts {
		m[f.Index] = f
	}
	return m
}

func sameForward(a, b Forward) bool {
	return a.Index == b.Index && a.ToClient == b.ToClient && sameSocket(a.Expected, b.Expected) &&
		sameSocket(a.NewSource, b.NewSource) && sameSocket(a.Dest, b.Dest)
}

func TestCompletePushesBothDirections(t *testing.T) {
	c, _, s := sinkTrack(t)
	client, server := sock("198.51.100.7", 40000), sock("10.0.1.5", 51820)
	if !c.AddPartial(1, client, server, "g") {
		t.Fatal("AddPartial")
	}
	if len(s.upserts) != 0 {
		t.Fatalf("a partial pushes nothing: %v", s.upserts)
	}
	if !c.Complete(2, 1, client, server) {
		t.Fatal("Complete")
	}
	if len(s.upserts) != 2 {
		t.Fatalf("upserts: %v", s.upserts)
	}
	got := lastByIndex(s)
	if want := (Forward{Index: 2, Expected: client, Dest: server, NewSource: probeSrc}); !sameForward(got[2], want) {
		t.Fatalf("towards server: %+v", got[2])
	}
	if want := (Forward{Index: 1, Expected: server, Dest: client, NewSource: entrySrc, ToClient: true}); !sameForward(got[1], want) {
		t.Fatalf("towards client: %+v", got[1])
	}
}

func TestSourceUnknownSkipsUpsertAndCounts(t *testing.T) {
	c, _ := clockTrack(DefaultLimits())
	s := &fakeSink{}
	c.SetSink(s, fakeSource{ok: false})
	session(t, c, 1, 2, sock("1.1.1.1", 1000), sock("10.0.0.1", 51820))
	if len(s.upserts) != 0 || c.SinkErrors() != 2 {
		t.Fatalf("upserts %v errors %d", s.upserts, c.SinkErrors())
	}
	if _, ok := c.LookupForward(2, sock("1.1.1.1", 1000)); !ok {
		t.Fatal("the slow path keeps working")
	}
}

func TestSinkErrorNeverFailsTheHandshake(t *testing.T) {
	c, _, s := sinkTrack(t)
	s.err = errors.New("map full")
	session(t, c, 1, 2, sock("1.1.1.1", 1000), sock("10.0.0.1", 51820))
	if c.SinkErrors() != 2 {
		t.Fatalf("errors %d", c.SinkErrors())
	}
}

func TestRoamUpdatesBothDirections(t *testing.T) {
	c, now, s := sinkTrack(t)
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	session(t, c, 10, 20, client, server)
	s.upserts = nil
	if _, ok := c.LookupForward(20, sock("6.6.6.6", 666)); ok {
		t.Fatal("illegal roam")
	}
	if len(s.upserts) != 0 {
		t.Fatalf("an illegal roam pushes nothing: %v", s.upserts)
	}
	*now = now.Add(6 * time.Second)
	roamed := sock("4.4.4.4", 4444)
	if _, ok := c.LookupForward(20, roamed); !ok {
		t.Fatal("legal roam")
	}
	if len(s.upserts) != 2 {
		t.Fatalf("upserts: %v", s.upserts)
	}
	got := lastByIndex(s)
	if !sameSocket(got[20].Expected, roamed) || !sameSocket(got[10].Dest, roamed) || !got[10].ToClient {
		t.Fatalf("roamed forwards: %+v %+v", got[20], got[10])
	}
}

func TestEvictionDeletesBothIndices(t *testing.T) {
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	both := func(t *testing.T, s *fakeSink, a, b uint32) {
		t.Helper()
		if len(s.deletes) != 2 || (s.deletes[0] != a || s.deletes[1] != b) && (s.deletes[0] != b || s.deletes[1] != a) {
			t.Fatalf("deletes: %v", s.deletes)
		}
	}
	t.Run("cleanup", func(t *testing.T) {
		c, now, s := sinkTrack(t)
		session(t, c, 1, 2, client, server)
		*now = now.Add(conntrackTTL + time.Second)
		if c.Cleanup() != 2 {
			t.Fatal("cleanup")
		}
		both(t, s, 1, 2)
	})
	t.Run("group", func(t *testing.T) {
		c, _, s := sinkTrack(t)
		c.AddPartial(1, client, server, "g")
		c.Complete(2, 1, client, server)
		c.RemoveGroup("g")
		both(t, s, 1, 2)
	})
	t.Run("takeover", func(t *testing.T) {
		c, now, s := sinkTrack(t)
		session(t, c, 1, 2, client, server)
		*now = now.Add(conntrackTTL + time.Second)
		if !c.AddPartial(2, sock("2.2.2.2", 2000), server) {
			t.Fatal("a stale index is taken over")
		}
		both(t, s, 1, 2)
	})
	t.Run("partial", func(t *testing.T) {
		c, now, s := sinkTrack(t)
		c.AddPartial(1, client, server)
		*now = now.Add(time.Minute)
		c.Cleanup()
		if len(s.deletes) != 0 {
			t.Fatalf("a partial was never pushed: %v", s.deletes)
		}
	})
}

func TestSeenKeepsFastPathSessionAlive(t *testing.T) {
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	c, now, _ := sinkTrack(t)
	session(t, c, 1, 2, client, server)
	*now = now.Add(2*time.Minute + 50*time.Second)
	c.Seen(map[uint32]time.Time{2: *now})
	*now = now.Add(20 * time.Second)
	if c.Cleanup() != 0 || c.Len() != 2 {
		t.Fatal("the session seen by the data plane is kept")
	}
	// an older time never moves it back: 20s + 2m39s is still inside the TTL of the Seen time
	c.Seen(map[uint32]time.Time{2: now.Add(-time.Hour), 1: now.Add(-time.Hour)})
	*now = now.Add(conntrackTTL - 25*time.Second)
	if c.Cleanup() != 0 {
		t.Fatal("an older time must not move LastSeen back")
	}

	c2, now2, _ := sinkTrack(t)
	session(t, c2, 1, 2, client, server)
	*now2 = now2.Add(3*time.Minute + 10*time.Second)
	if c2.Cleanup() != 2 {
		t.Fatal("without Seen the session is removed")
	}
}

func TestRestoreRebuildsSession(t *testing.T) {
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	c, now, s := sinkTrack(t)
	if !c.Restore(1, 2, client, server, "g", *now, 0) {
		t.Fatal("Restore")
	}
	if len(s.upserts) != 0 {
		t.Fatal("the data plane has it already")
	}
	if dst, ok := c.LookupForward(2, client); !ok || !sameSocket(dst, server) {
		t.Fatalf("lookup: %v %v", dst, ok)
	}
	if dst, ok := c.LookupForward(1, server); !ok || !sameSocket(dst, client) {
		t.Fatalf("reverse lookup: %v %v", dst, ok)
	}
	if got := c.Sessions(); len(got) != 2 || got[0].Index != 1 || got[1].Index != 2 {
		t.Fatalf("sessions: %+v", got)
	}
	c.RemoveGroup("g")
	if c.Len() != 0 {
		t.Fatal("the restored group is removable")
	}

	// an index owned by a live different session
	c, now, _ = sinkTrack(t)
	session(t, c, 1, 2, client, server)
	if c.Restore(3, 2, sock("2.2.2.2", 2), server, "g", *now, 0) || c.Restore(1, 4, sock("2.2.2.2", 2), server, "g", *now, 0) {
		t.Fatal("a live index is not taken")
	}
	// a full table
	l := DefaultLimits()
	l.MaxEntries = 2
	c, now = clockTrack(l)
	session(t, c, 1, 2, client, server)
	if c.Restore(3, 4, client, server, "g", *now, 0) {
		t.Fatal("a full table refuses")
	}
}

func TestNoSinkIsUserspace(t *testing.T) {
	c, _ := clockTrack(DefaultLimits())
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	session(t, c, 1, 2, client, server)
	c.UpdateRoaming(2, sock("3.3.3.3", 3))
	c.RemoveGroup("")
	if c.SinkErrors() != 0 || c.Len() != 0 {
		t.Fatal("no sink: nothing to count")
	}
}

func TestRestoreOverStaleSessionKeepsTheDataPlaneEntries(t *testing.T) {
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	c, now, s := sinkTrack(t)
	session(t, c, 1, 2, client, server)
	session(t, c, 5, 6, sock("3.3.3.3", 3), server) // 6 pairs with 5 only
	*now = now.Add(conntrackTTL + time.Second)
	s.deletes, s.upserts = nil, nil
	if !c.Restore(1, 2, client, server, "g", *now, 0) {
		t.Fatal("Restore over a stale session")
	}
	if len(s.deletes) != 0 {
		t.Fatalf("the restored indices must stay in the data plane: %v", s.deletes)
	}
	if got := c.Sessions(); len(got) != 2 || got[0].Index != 1 || got[1].Index != 2 {
		t.Fatalf("sessions: %+v", got)
	}
	// a stale owner paired with an index outside the restore: the peer is removed and deleted from the sink
	c, now, s = sinkTrack(t)
	session(t, c, 1, 2, client, server)
	*now = now.Add(conntrackTTL + time.Second)
	s.deletes = nil
	if !c.Restore(1, 9, client, server, "g", *now, 0) {
		t.Fatal("Restore")
	}
	if len(s.deletes) != 1 || s.deletes[0] != 2 {
		t.Fatalf("only the abandoned peer is deleted: %v", s.deletes)
	}
}

func TestRestoreIsAtomic(t *testing.T) {
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	c, now, s := sinkTrack(t)
	session(t, c, 1, 2, client, server)
	*now = now.Add(conntrackTTL - time.Second)
	session(t, c, 7, 8, sock("3.3.3.3", 3), server) // live
	*now = now.Add(2 * time.Second)                 // 1/2 stale now, 7/8 live
	s.deletes = nil
	if c.Restore(1, 8, client, server, "g", *now, 0) {
		t.Fatal("a live second index refuses the restore")
	}
	if len(s.deletes) != 0 || c.Len() != 4 {
		t.Fatalf("nothing changed: deletes %v len %d", s.deletes, c.Len())
	}
	if _, ok := c.entries[1]; !ok {
		t.Fatal("the stale owner of the first index is untouched")
	}
}

func TestSeenClampsAFutureTime(t *testing.T) {
	c, now, _ := sinkTrack(t)
	session(t, c, 1, 2, sock("1.1.1.1", 1000), sock("10.0.0.1", 51820))
	*now = now.Add(time.Minute)
	c.Seen(map[uint32]time.Time{2: now.Add(24 * time.Hour)})
	*now = now.Add(conntrackTTL + time.Second)
	if c.Cleanup() != 2 {
		t.Fatal("a time in the future must not keep a session alive for long")
	}
}

// The fast path's contract: both directions name the client -> server index as their session, the reply direction carries the
// address the server sends to (the demux socket's source towards it) and the device the server's answer arrived on.
func TestForwardsCarrySessionViaAndReplyDevice(t *testing.T) {
	c, _, s := sinkTrack(t)
	client, server := sock("198.51.100.7", 40000), sock("10.0.1.5", 51820)
	if !c.AddPartial(1, client, server, "g") || !c.CompleteFrom(2, 1, client, server, 17) {
		t.Fatal("handshake")
	}
	got := lastByIndex(s)
	if got[2].Session != 2 || got[1].Session != 2 {
		t.Fatalf("sessions: %d %d, want 2 2", got[2].Session, got[1].Session)
	}
	if got[1].ReplyIfindex != 17 || got[2].ReplyIfindex != 0 {
		t.Fatalf("reply devices: %d %d", got[1].ReplyIfindex, got[2].ReplyIfindex)
	}
	if !sameSocket(got[1].Via, probeSrc) || got[2].Via.IP != nil {
		t.Fatalf("via: %+v %+v", got[1].Via, got[2].Via)
	}
	// a roam pushes the device again; Sessions reports it too
	s.upserts = nil
	c.UpdateRoaming(1, sock("4.4.4.4", 4))
	if got := lastByIndex(s); got[1].ReplyIfindex != 17 {
		t.Fatalf("after a roam: %+v", got[1])
	}
	for _, f := range c.Sessions() {
		if f.ToClient && f.ReplyIfindex != 17 {
			t.Fatalf("sessions: %+v", f)
		}
	}
}

// Without the source towards the server the reply direction has no via address: neither direction is written.
func TestServerSourceUnknownWritesNeitherDirection(t *testing.T) {
	c, _ := clockTrack(DefaultLimits())
	s := &fakeSink{}
	c.SetSink(s, partialSource{})
	session(t, c, 1, 2, sock("1.1.1.1", 1000), sock("10.0.0.1", 51820))
	if len(s.upserts) != 0 || c.SinkErrors() != 2 {
		t.Fatalf("upserts %v errors %d", s.upserts, c.SinkErrors())
	}
}

type partialSource struct{}

func (partialSource) TowardsServer(Socket) (Socket, bool) { return Socket{}, false }
func (partialSource) TowardsClient(Socket) (Socket, bool) { return entrySrc, true }

// A handshake refused because its index belongs to another live session writes nothing: the fast path's record of that
// index stays the live session's.
func TestRefusedHandshakeWritesNothing(t *testing.T) {
	c, _, s := sinkTrack(t)
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	session(t, c, 1, 2, client, server)
	s.upserts, s.deletes = nil, nil
	other := sock("2.2.2.2", 2000)
	if c.AddPartial(1, other, server, "g") {
		t.Fatal("a live client index is taken")
	}
	if !c.AddPartial(3, other, server) || c.CompleteFrom(2, 3, other, server, 9) {
		t.Fatal("a live server index is taken")
	}
	if c.AddPartial(2, other, server) {
		t.Fatal("a live server index is taken as a client index")
	}
	if len(s.upserts) != 0 || len(s.deletes) != 0 {
		t.Fatalf("a refused handshake wrote upserts %v deletes %v", s.upserts, s.deletes)
	}
}

func TestRestoreKeepsTheReplyDevice(t *testing.T) {
	c, now, s := sinkTrack(t)
	client, server := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	if !c.Restore(1, 2, client, server, "g", *now, 23) {
		t.Fatal("Restore")
	}
	c.UpdateRoaming(1, sock("4.4.4.4", 4))
	if got := lastByIndex(s); got[1].ReplyIfindex != 23 {
		t.Fatalf("restored reply device lost: %+v", got[1])
	}
	if c.SessionCount() != 1 {
		t.Fatalf("session count %d", c.SessionCount())
	}
}
