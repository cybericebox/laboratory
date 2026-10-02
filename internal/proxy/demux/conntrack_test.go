package demux

import (
	"net"
	"testing"
	"time"
)

func sock(ip string, port uint16) Socket { return Socket{IP: net.ParseIP(ip), Port: port} }

// clock is a conntrack whose time the test moves.
func clockTrack(l Limits) (*ConnTrack, *time.Time) {
	now := time.Unix(1_000_000, 0)
	c := NewConnTrackWithLimits(l)
	c.now = func() time.Time { return now }
	return c, &now
}

func session(t *testing.T, c *ConnTrack, ci, si uint32, client, backend Socket) {
	t.Helper()
	if !c.AddPartial(ci, client, backend) {
		t.Fatalf("AddPartial %d refused", ci)
	}
	if !c.Complete(si, ci, client, backend) {
		t.Fatalf("Complete %d refused", si)
	}
}

// An index that a live session owns is never taken by another handshake.
func TestIndexCollisionIsRefused(t *testing.T) {
	c, _ := clockTrack(DefaultLimits())
	session(t, c, 10, 20, sock("1.1.1.1", 1000), sock("10.0.0.1", 51820))
	if c.AddPartial(10, sock("2.2.2.2", 2000), sock("10.0.0.1", 51820)) {
		t.Error("another client must not take the index 10")
	}
	if c.AddPartial(20, sock("2.2.2.2", 2000), sock("10.0.0.1", 51820)) {
		t.Error("another client must not take the index 20")
	}
	// the same client retransmitting its init is fine
	if !c.AddPartial(10, sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)) {
		t.Error("a retransmitted init of the same client is accepted")
	}
	// a server answer with an index that belongs to another session is refused
	if !c.AddPartial(30, sock("3.3.3.3", 3000), sock("10.0.0.1", 51820)) {
		t.Fatal("AddPartial 30")
	}
	if c.Complete(20, 30, sock("3.3.3.3", 3000), sock("10.0.0.1", 51820)) {
		t.Error("Si 20 belongs to another live session")
	}
}

// Roaming works, but a session changes address at most once per interval, and a packet from a third address inside the
// interval neither forwards nor moves anything.
func TestRoamingIsRateLimited(t *testing.T) {
	c, now := clockTrack(DefaultLimits())
	client, backend := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	session(t, c, 10, 20, client, backend)
	// client-to-server packets carry the server's index 20
	if dst, ok := c.LookupForward(20, client); !ok || !sameSocket(dst, backend) {
		t.Fatalf("normal forward: %v %v", dst, ok)
	}
	// right after the handshake a spoofed source is not let to move the session
	if _, ok := c.LookupForward(20, sock("6.6.6.6", 666)); ok {
		t.Fatal("a roam within the interval must not be forwarded")
	}
	if exp, _ := c.Expected(20); !sameSocket(exp, client) {
		t.Fatalf("the session must not have moved: %v", exp)
	}
	// later the client really roams
	*now = now.Add(6 * time.Second)
	roamed := sock("4.4.4.4", 4444)
	if dst, ok := c.LookupForward(20, roamed); !ok || !sameSocket(dst, backend) {
		t.Fatalf("roaming after the interval: %v %v", dst, ok)
	}
	if exp, _ := c.Expected(20); !sameSocket(exp, roamed) {
		t.Fatal("the session moved to the new address")
	}
	// the reverse direction now goes to the new address
	if dst, ok := c.LookupForward(10, backend); !ok || !sameSocket(dst, roamed) {
		t.Fatalf("server to client after roaming: %v %v", dst, ok)
	}
	// and an immediate second change is refused
	if _, ok := c.LookupForward(20, sock("7.7.7.7", 7)); ok {
		t.Fatal("a second change within the interval is refused")
	}
}

// Cookie replies are forwarded only from the exact expected source, and never move the session.
func TestStrictLookupForCookieReplies(t *testing.T) {
	c, _ := clockTrack(DefaultLimits())
	client, backend := sock("1.1.1.1", 1000), sock("10.0.0.1", 51820)
	if !c.AddPartial(10, client, backend) {
		t.Fatal()
	}
	// the cookie reply of the VPN pod names the client's index
	if dst, ok := c.LookupStrict(10, backend); !ok || !sameSocket(dst, client) {
		t.Fatalf("from the backend: %v %v", dst, ok)
	}
	if _, ok := c.LookupStrict(10, sock("9.9.9.9", 9)); ok {
		t.Fatal("a forged cookie reply must not be forwarded")
	}
	if exp, _ := c.Expected(10); !sameSocket(exp, backend) {
		t.Fatal("a cookie reply never moves the session")
	}
}

// The table is capped as a whole and per client address; room returns when entries expire.
func TestEntryCaps(t *testing.T) {
	l := DefaultLimits()
	l.MaxEntries, l.MaxEntriesPerSource = 8, 4
	c, now := clockTrack(l)
	backend := sock("10.0.0.1", 51820)
	for i := uint32(0); i < 2; i++ { // two sessions = 4 entries for one address
		session(t, c, 100+i, 200+i, sock("1.1.1.1", 1000+uint16(i)), backend)
	}
	if c.AddPartial(300, sock("1.1.1.1", 1999), backend) {
		t.Fatal("a third session of one address passes the per-source cap")
	}
	// other addresses still fit, up to the global cap
	session(t, c, 400, 500, sock("2.2.2.2", 1), backend)
	session(t, c, 401, 501, sock("3.3.3.3", 1), backend)
	if c.Len() != 8 {
		t.Fatalf("entries = %d", c.Len())
	}
	if c.AddPartial(402, sock("4.4.4.4", 1), backend) {
		t.Fatal("the global cap must hold")
	}
	*now = now.Add(conntrackTTL + time.Second)
	if n := c.Cleanup(); n != 8 {
		t.Fatalf("cleanup removed %d", n)
	}
	if c.Len() != 0 || len(c.perSrc) != 0 {
		t.Fatalf("everything is gone: %d entries, %v", c.Len(), c.perSrc)
	}
	if !c.AddPartial(402, sock("4.4.4.4", 1), backend) {
		t.Fatal("room returns after expiry")
	}
}

func TestLimiterRefillsAndForgetsIdleSources(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(2, 3, 2)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatalf("burst %d", i)
		}
	}
	if l.allow("a") {
		t.Fatal("the burst is spent")
	}
	now = now.Add(time.Second) // two tokens come back
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Fatal("refill at the rate")
	}
	// the table of sources is bounded, and a new source is never refused for that: the one quiet longest is forgotten
	l.allow("b")
	now = now.Add(time.Second)
	if !l.allow("c") {
		t.Fatal("a new source must be admitted when the table is full")
	}
	if len(l.buckets) != 2 {
		t.Fatalf("the table stays bounded: %d", len(l.buckets))
	}
	now = now.Add(time.Hour)
	l.sweep()
	if len(l.buckets) != 0 || !l.allow("c") {
		t.Fatal("idle sources are forgotten")
	}
}

// R-16: a session, and all the sessions of one client address, have a packet rate of their own.
func TestSessionAndOwnerPacketRates(t *testing.T) {
	now := time.Unix(1000, 0)
	l := DefaultLimits()
	l.SessionRate, l.SessionBurst = 10, 3
	l.OwnerRate, l.OwnerBurst = 100, 100
	c := NewConnTrackWithLimits(l)
	c.now = func() time.Time { return now }
	client := Socket{IP: net.ParseIP("203.0.113.5"), Port: 4000}
	backend := Socket{IP: net.ParseIP("10.0.0.9"), Port: 51820}
	if !c.AddPartial(1, client, backend) || !c.Complete(2, 1, client, backend) {
		t.Fatal("setup")
	}
	ok := 0
	for i := 0; i < 10; i++ {
		if _, found := c.LookupForward(2, client); found {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("the session burst is 3, %d packets passed", ok)
	}
	now = now.Add(time.Second) // ten tokens come back, the bucket holds three
	ok = 0
	for i := 0; i < 10; i++ {
		if _, found := c.LookupForward(2, client); found {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("after a second %d packets passed", ok)
	}
	// the owner's bucket is shared by its sessions: a second session of the same address passes only what is left of it
	l2 := DefaultLimits()
	l2.SessionRate, l2.SessionBurst = 0, 0
	l2.OwnerRate, l2.OwnerBurst = 10, 4
	c2 := NewConnTrackWithLimits(l2)
	c2.now = func() time.Time { return now }
	for _, ci := range []uint32{10, 20} {
		if !c2.AddPartial(ci, client, backend) || !c2.Complete(ci+1, ci, client, backend) {
			t.Fatal("setup 2")
		}
	}
	passed := 0
	for i := 0; i < 5; i++ {
		for _, si := range []uint32{11, 21} {
			if _, found := c2.LookupForward(si, client); found {
				passed++
			}
		}
	}
	if passed != 4 {
		t.Fatalf("the owner's burst is 4 across both sessions, %d passed", passed)
	}
}
