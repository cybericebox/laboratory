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

// The table is capped as a whole, and there is no cap per client address (one NAT hides a whole event); room returns when entries expire.
func TestEntryCaps(t *testing.T) {
	l := DefaultLimits()
	l.MaxEntries = 8
	c, now := clockTrack(l)
	backend := sock("10.0.0.1", 51820)
	for i := uint32(0); i < 4; i++ { // four sessions = 8 entries, all of one address: nothing stops a venue behind one NAT
		session(t, c, 100+i, 200+i, sock("1.1.1.1", 1000+uint16(i)), backend)
	}
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
	if c.Len() != 0 {
		t.Fatalf("everything is gone: %d entries", c.Len())
	}
	if !c.AddPartial(402, sock("4.4.4.4", 1), backend) {
		t.Fatal("room returns after expiry")
	}
}

// D-1: 300 clients behind one address (a venue's NAT) all get their sessions.
func TestManyClientsBehindOneAddress(t *testing.T) {
	c, _ := clockTrack(DefaultLimits())
	backend := sock("10.0.0.1", 51820)
	for i := uint32(0); i < 300; i++ {
		session(t, c, 1000+i, 5000+i, sock("203.0.113.7", 2000+uint16(i)), backend)
	}
	if c.Len() != 600 {
		t.Fatalf("entries = %d, want 600", c.Len())
	}
}

// D-2: a handshake in progress (init forwarded, never answered) expires within PartialTTL, a completed session keeps its entry for conntrackTTL.
func TestPartialEntriesExpireWithinSeconds(t *testing.T) {
	l := DefaultLimits()
	l.PartialTTL = 15 * time.Second
	c, now := clockTrack(l)
	backend := sock("10.0.0.1", 51820)
	session(t, c, 10, 20, sock("1.1.1.1", 1000), backend)
	for i := uint32(0); i < 50; i++ { // spoofed inits that are never answered
		if !c.AddPartial(100+i, sock("9.9.9.9", 1000+uint16(i)), backend) {
			t.Fatalf("AddPartial %d", i)
		}
	}
	if c.Len() != 52 {
		t.Fatalf("entries = %d", c.Len())
	}
	*now = now.Add(14 * time.Second)
	if n := c.Cleanup(); n != 0 {
		t.Fatalf("nothing is stale after 14s, %d removed", n)
	}
	*now = now.Add(2 * time.Second)
	if n := c.Cleanup(); n != 50 {
		t.Fatalf("the 50 unanswered inits must go after PartialTTL, %d removed", n)
	}
	if c.Len() != 2 {
		t.Fatalf("the completed session stays: %d entries", c.Len())
	}
	if _, ok := c.LookupSender(10); !ok {
		t.Fatal("the completed session is still there")
	}
	*now = now.Add(conntrackTTL)
	if c.Cleanup(); c.Len() != 0 {
		t.Fatalf("the idle session goes after conntrackTTL: %d entries", c.Len())
	}
}

// D-2: an expired partial entry no longer blocks its index, and answering a fresh one completes it into a session with the long TTL.
func TestPartialIndexIsFreeAfterTTLAndCompletionTurnsItIntoASession(t *testing.T) {
	l := DefaultLimits()
	l.PartialTTL = 10 * time.Second
	c, now := clockTrack(l)
	backend := sock("10.0.0.1", 51820)
	if !c.AddPartial(7, sock("1.1.1.1", 1), backend) {
		t.Fatal("first init")
	}
	if c.AddPartial(7, sock("2.2.2.2", 2), backend) {
		t.Fatal("a live partial entry owns its index")
	}
	*now = now.Add(11 * time.Second)
	if !c.AddPartial(7, sock("2.2.2.2", 2), backend) {
		t.Fatal("the stale partial entry must not block its index")
	}
	if !c.Complete(8, 7, sock("2.2.2.2", 2), backend) {
		t.Fatal("complete")
	}
	*now = now.Add(2 * time.Minute) // far past PartialTTL, inside conntrackTTL
	if n := c.Cleanup(); n != 0 {
		t.Fatalf("a completed session is not partial any more: %d removed", n)
	}
}

func TestLimiterRefills(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(2, 3)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.allow() {
			t.Fatalf("burst %d", i)
		}
	}
	if l.allow() {
		t.Fatal("the burst is spent")
	}
	now = now.Add(time.Second) // two tokens come back
	if !l.allow() || !l.allow() || l.allow() {
		t.Fatal("refill at the rate")
	}
	if !newLimiter(0, 0).allow() {
		t.Fatal("rate 0 is unlimited")
	}
}

// R-16: a session has a packet rate of its own.
func TestSessionPacketRate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := DefaultLimits()
	l.SessionRate, l.SessionBurst = 10, 3
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
	// a second session of the same address has its own bucket: nothing is shared per address
	if !c.AddPartial(10, client, backend) || !c.Complete(11, 10, client, backend) {
		t.Fatal("setup 2")
	}
	ok = 0
	for i := 0; i < 10; i++ {
		if _, found := c.LookupForward(11, client); found {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("the second session has its own burst of 3, %d passed", ok)
	}
}
