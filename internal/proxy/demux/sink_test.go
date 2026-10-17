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
	if !c.Restore(1, 2, client, server, "g", *now) {
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
	if c.Restore(3, 2, sock("2.2.2.2", 2), server, "g", *now) || c.Restore(1, 4, sock("2.2.2.2", 2), server, "g", *now) {
		t.Fatal("a live index is not taken")
	}
	// a full table
	l := DefaultLimits()
	l.MaxEntries = 2
	c, now = clockTrack(l)
	session(t, c, 1, 2, client, server)
	if c.Restore(3, 4, client, server, "g", *now) {
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
