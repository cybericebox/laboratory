package deviceaccess

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

func testOpen() wire.CableOpen {
	return wire.CableOpen{Namespace: testNS, Name: "cable-1", UID: "cable-uid", Lab: testLab, LabUID: "lab-uid",
		Serial: wire.PortRef{Device: "pc1", Port: "serial0"}, Console: wire.PortRef{Device: testDev, Port: "console0"}}
}

func testAuth(now time.Time) *cableAuth {
	return &cableAuth{id: testIDs["n2"], nonces: newNonceCache(100, 50), now: func() time.Time { return now }, started: now.Add(-time.Hour)}
}

// An open signed by the identity the console end's node published passes once. A replay, a changed field, a signature of another node
// (a compromised node cannot sign as another one: there is no shared secret), a request outside the window and one from before the start
// of the process all fail.
func TestCableAuth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := testAuth(now)
	n1 := testIDs["n1"].PublicKey()
	req, _, err := signOpen(testIDs["n1"], "n1", testOpen(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.check(req, n1); err != nil {
		t.Fatalf("a signed request: %v", err)
	}
	if err := a.check(req, n1); !errors.Is(err, errCableReplay) {
		t.Fatalf("a replayed request: %v", err)
	}
	for name, mod := range map[string]func(*wire.CableOpen){
		"another lab":         func(r *wire.CableOpen) { r.Lab = "lab2" },
		"another serial port": func(r *wire.CableOpen) { r.Serial.Port = "serial1" },
		"another cable uid":   func(r *wire.CableOpen) { r.UID = "x" },
		"another node named":  func(r *wire.CableOpen) { r.Node = "n2" },
		"another ephemeral":   func(r *wire.CableOpen) { e, _ := newEphemeral(); r.Eph = e.public() },
	} {
		fresh, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now)
		mod(&fresh)
		if err := a.check(fresh, n1); !errors.Is(err, errCableSig) {
			t.Errorf("%s after signing: %v", name, err)
		}
	}
	// n3 signs an open that names n1: the key n1 published does not verify it
	forged, _, _ := signOpen(testIDs["n3"], "n1", testOpen(), now)
	if err := a.check(forged, n1); !errors.Is(err, errCableSig) {
		t.Fatalf("an impersonation: %v", err)
	}
	if err := a.check(req, "not-a-key"); err == nil {
		t.Fatal("no published key")
	}
	old, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now.Add(-CableWindow-time.Second))
	if err := a.check(old, n1); !errors.Is(err, errCableTime) {
		t.Fatalf("an old request: %v", err)
	}
	future, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now.Add(CableWindow+time.Second))
	if err := a.check(future, n1); !errors.Is(err, errCableTime) {
		t.Fatalf("a request from the future: %v", err)
	}
	restarted := testAuth(now)
	restarted.started = now
	before, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now.Add(-time.Second))
	if err := restarted.check(before, n1); !errors.Is(err, errCableTime) {
		t.Fatalf("an open from before the start of the process: %v", err)
	}
}

// The reply is bound to the open (its digest), the nonce and the acceptor's identity.
func TestCableReply(t *testing.T) {
	open := openTranscript(testOpen())
	e, _ := newEphemeral()
	r := signReply(testIDs["n2"], open, wire.CableReply{OK: true, Nonce: "n1", Accept: strings.Repeat("a", 32), Eph: e.public()})
	n2 := testIDs["n2"].PublicKey()
	if err := checkReply(n2, open, r, "n1"); err != nil {
		t.Fatal(err)
	}
	if checkReply(n2, open, r, "other") == nil {
		t.Fatal("a reply to another nonce")
	}
	if checkReply(n2, append([]byte("x"), open...), r, "n1") == nil {
		t.Fatal("a reply to another open")
	}
	if checkReply(testIDs["n3"].PublicKey(), open, r, "n1") == nil {
		t.Fatal("a reply of another node")
	}
	changed := r
	changed.Eph = testIDs["n3"].PublicKey()
	if checkReply(n2, open, changed, "n1") == nil {
		t.Fatal("a reply whose ephemeral key was swapped")
	}
}

// Both sides of an X25519 exchange agree on the secret; a low-order (all-zero) peer key is refused.
func TestEphemeralAgreement(t *testing.T) {
	a, _ := newEphemeral()
	b, _ := newEphemeral()
	sa, err1 := a.shared(b.public())
	sb, err2 := b.shared(a.public())
	if err1 != nil || err2 != nil || string(sa) != string(sb) {
		t.Fatal("no agreement")
	}
	if _, err := a.shared(b64.EncodeToString(make([]byte, 32))); err == nil {
		t.Fatal("a low-order key")
	}
}

// One node cannot fill the cache for the others: it loses its own oldest nonces; a full cache loses the oldest one, it never refuses. A
// forgotten nonce is not replayable inside its window: an open of that node that is not newer than the newest forgotten one is refused.
func TestNonceCache(t *testing.T) {
	c := newNonceCache(4, 2)
	now := time.Unix(100, 0)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	if !c.add("a", "n1", ms(0), now) || !c.add("b", "n1", ms(1*time.Millisecond), now) || c.add("a", "n1", ms(0), now) {
		t.Fatal("first nonces")
	}
	if !c.add("c", "n1", ms(2*time.Millisecond), now) {
		t.Fatal("a node over its quota loses its oldest nonce, not the new one")
	}
	if _, ok := c.seen["a"]; ok || c.bySource["n1"] != 2 {
		t.Fatalf("quota: %v", c.seen)
	}
	// "a" was forgotten: replaying it is refused all the same
	if c.add("a", "n1", ms(0), now) {
		t.Fatal("a forgotten nonce replayed")
	}
	if !c.add("d", "n2", ms(0), now) || !c.add("e", "n3", ms(0), now) {
		t.Fatal("other nodes still get room")
	}
	if !c.add("f", "n4", ms(3*time.Millisecond), now) || len(c.seen) != 4 {
		t.Fatalf("a full cache: %d", len(c.seen))
	}
	if !c.add("g", "n5", ms(2*CableWindow+time.Second), now.Add(2*CableWindow+time.Second)) || len(c.seen) != 1 {
		t.Fatalf("old nonces must be forgotten: %v", c.seen)
	}
}

func TestPublicKeyShape(t *testing.T) {
	id, _ := NewIdentity()
	if len(id.PublicKey()) != 43 {
		t.Fatalf("%q", id.PublicKey())
	}
	if _, err := ParsePublicKey(id.PublicKey()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x", strings.Repeat("A", 44), strings.Repeat("+", 43)} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("%q", bad)
		}
	}
}

// A dialer's clock may run ahead of the acceptor's: an open it signed before the acceptor restarted can carry a time after the start.
// Once the node has opened a stream to this process, every open it signed before that one is refused.
func TestCableAuthRefusesOpensSignedBeforeTheFirstAccepted(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := testAuth(now)
	a.started = now.Add(-5 * time.Second)
	n1 := testIDs["n1"].PublicKey()
	captured, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now.Add(-2*time.Second)) // signed (dialer ahead) before the restart
	fresh, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now)
	if err := a.check(fresh, n1); err != nil {
		t.Fatal(err)
	}
	if err := a.check(captured, n1); !errors.Is(err, errCableReplay) {
		t.Fatalf("an open signed before the first accepted one: %v", err)
	}
	later, _, _ := signOpen(testIDs["n1"], "n1", testOpen(), now.Add(time.Second))
	if err := a.check(later, n1); err != nil {
		t.Fatalf("a later open: %v", err)
	}
	other, _, _ := signOpen(testIDs["n3"], "n3", testOpen(), now.Add(-time.Second))
	if err := a.check(other, testIDs["n3"].PublicKey()); err != nil {
		t.Fatalf("the floor is per node: %v", err)
	}
}
