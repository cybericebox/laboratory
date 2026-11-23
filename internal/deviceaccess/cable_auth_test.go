package deviceaccess

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

var (
	testCableKey = []byte(strings.Repeat("k", 48))
	oldCableKey  = []byte(strings.Repeat("o", 48))
)

func testOpen() wire.CableOpen {
	return wire.CableOpen{Namespace: testNS, Name: "cable-1", UID: "cable-uid", Lab: testLab, LabUID: "lab-uid",
		Serial: wire.PortRef{Device: "pc1", Port: "serial0"}, Console: wire.PortRef{Device: testDev, Port: "console0"}}
}

func testAuth(now time.Time, keys ...[]byte) *cableAuth {
	return &cableAuth{key: StaticCableKey(keys...), nonces: newNonceCache(100, 50), now: func() time.Time { return now }}
}

// An open request signed with the node key of its node passes once; a replay, a changed field, another node named than the one that
// signed, another key, a request outside the window and a request without a key all fail.
func TestCableAuth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := testAuth(now, testCableKey)
	req, key, err := signOpen(testCableKey, "n1", testOpen(), now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.check(req, "10.0.0.1")
	if err != nil || string(got) != string(key) || string(key) != string(nodeKey(testCableKey, "n1")) {
		t.Fatalf("a signed request: %v", err)
	}
	if _, err := a.check(req, "10.0.0.1"); !errors.Is(err, errCableReplay) {
		t.Fatalf("a replayed request: %v", err)
	}
	for name, mod := range map[string]func(*wire.CableOpen){
		"another lab":         func(r *wire.CableOpen) { r.Lab = "lab2" },
		"another namespace":   func(r *wire.CableOpen) { r.Namespace = "other" },
		"another serial port": func(r *wire.CableOpen) { r.Serial.Port = "serial1" },
		"another cable uid":   func(r *wire.CableOpen) { r.UID = "x" },
		"another node named":  func(r *wire.CableOpen) { r.Node = "n2" },
	} {
		fresh, _, _ := signOpen(testCableKey, "n1", testOpen(), now)
		mod(&fresh)
		if _, err := a.check(fresh, "10.0.0.1"); !errors.Is(err, errCableMAC) {
			t.Errorf("%s after signing: %v", name, err)
		}
	}
	forged, _, _ := signOpen([]byte(strings.Repeat("x", 48)), "n1", testOpen(), now)
	if _, err := a.check(forged, "10.0.0.1"); !errors.Is(err, errCableMAC) {
		t.Fatalf("another key: %v", err)
	}
	old, _, _ := signOpen(testCableKey, "n1", testOpen(), now.Add(-CableWindow-time.Second))
	if _, err := a.check(old, "10.0.0.1"); !errors.Is(err, errCableTime) {
		t.Fatalf("an old request: %v", err)
	}
	future, _, _ := signOpen(testCableKey, "n1", testOpen(), now.Add(CableWindow+time.Second))
	if _, err := a.check(future, "10.0.0.1"); !errors.Is(err, errCableTime) {
		t.Fatalf("a request from the future: %v", err)
	}
	fresh, _, _ := signOpen([]byte("short"), "n1", testOpen(), now)
	if _, err := testAuth(now, []byte("short")).check(fresh, "10.0.0.1"); !errors.Is(err, errCableKey) {
		t.Fatalf("a short key must not be used: %v", err)
	}
	bad := req
	bad.Nonce = strings.Repeat("Z", 32)
	if _, err := a.check(bad, "10.0.0.1"); err == nil {
		t.Fatal("a nonce that is not hex")
	}
}

// During a rotation a request signed with the previous key is accepted too; once it is gone, only the new one.
func TestCableAuthRotation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	both := testAuth(now, testCableKey, oldCableKey)
	byOld, _, _ := signOpen(oldCableKey, "n1", testOpen(), now)
	key, err := both.check(byOld, "10.0.0.1")
	if err != nil || string(key) != string(nodeKey(oldCableKey, "n1")) {
		t.Fatalf("the previous key during the rotation: %v", err)
	}
	byNew, _, _ := signOpen(testCableKey, "n1", testOpen(), now)
	if _, err := both.check(byNew, "10.0.0.1"); err != nil {
		t.Fatalf("the new key: %v", err)
	}
	byOld2, _, _ := signOpen(oldCableKey, "n1", testOpen(), now)
	if _, err := testAuth(now, testCableKey).check(byOld2, "10.0.0.1"); !errors.Is(err, errCableMAC) {
		t.Fatalf("the previous key after the rotation: %v", err)
	}
}

// The reply is bound to the nonce of the request, the acceptor's nonce and the key.
func TestCableReply(t *testing.T) {
	accept := strings.Repeat("a", 32)
	r := signReply(testCableKey, wire.CableReply{OK: true, Nonce: "n1", Accept: accept})
	if err := checkReply(testCableKey, r, "n1"); err != nil {
		t.Fatal(err)
	}
	if checkReply(testCableKey, r, "n2") == nil {
		t.Fatal("a reply to another request")
	}
	changed := r
	changed.Accept = strings.Repeat("b", 32)
	if checkReply(testCableKey, changed, "n1") == nil {
		t.Fatal("another acceptor nonce")
	}
	r.OK = false
	if checkReply(testCableKey, r, "n1") == nil {
		t.Fatal("a changed reply")
	}
	if checkReply([]byte(strings.Repeat("z", 48)), signReply(testCableKey, wire.CableReply{Nonce: "n1"}), "n1") == nil {
		t.Fatal("another key")
	}
}

// One source cannot fill the cache for the others: it loses its own oldest nonces; a full cache loses the oldest one, it never refuses.
func TestNonceCache(t *testing.T) {
	c := newNonceCache(4, 2)
	now := time.Unix(100, 0)
	if !c.add("a", "s1", now) || !c.add("b", "s1", now.Add(time.Millisecond)) || c.add("a", "s1", now) {
		t.Fatal("first nonces")
	}
	if !c.add("c", "s1", now.Add(2*time.Millisecond)) {
		t.Fatal("a source over its quota loses its oldest nonce, not the new one")
	}
	if _, ok := c.seen["a"]; ok || c.bySource["s1"] != 2 {
		t.Fatalf("quota: %v", c.seen)
	}
	if !c.add("d", "s2", now) || !c.add("e", "s3", now) {
		t.Fatal("other sources still get room")
	}
	if !c.add("f", "s4", now.Add(3*time.Millisecond)) || len(c.seen) != 4 {
		t.Fatalf("a full cache: %d", len(c.seen))
	}
	if !c.add("g", "s5", now.Add(2*CableWindow+time.Second)) || len(c.seen) != 1 {
		t.Fatalf("old nonces must be forgotten: %v", c.seen)
	}
}

// The key files are read again when they change; the previous key may be absent.
func TestCableKeyFile(t *testing.T) {
	dir := t.TempDir()
	p, prev := filepath.Join(dir, "key"), filepath.Join(dir, "previous")
	if _, err := NewCableKey(p, prev).Keys(); err == nil {
		t.Fatal("no file")
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("a", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := NewCableKey(p, prev)
	got, err := k.Keys()
	if err != nil || len(got) != 1 || string(got[0]) != strings.Repeat("a", 40) {
		t.Fatalf("%q %v", got, err)
	}
	if err := os.WriteFile(prev, []byte(strings.Repeat("o", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("b", 41)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.Keys(); len(got) != 2 || string(got[0]) != strings.Repeat("b", 41) || string(got[1]) != strings.Repeat("o", 40) {
		t.Fatalf("a rotated key: %q", got)
	}
	_ = os.Remove(prev)
	if got, _ := k.Keys(); len(got) != 1 {
		t.Fatalf("the previous key removed: %q", got)
	}
}
