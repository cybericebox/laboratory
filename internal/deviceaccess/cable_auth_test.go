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

var testCableKey = []byte(strings.Repeat("k", 48))

func testOpen() wire.CableOpen {
	return wire.CableOpen{Namespace: testNS, Name: "cable-1", UID: "cable-uid", Lab: testLab, LabUID: "lab-uid",
		Serial: wire.PortRef{Device: "pc1", Port: "serial0"}, Console: wire.PortRef{Device: testDev, Port: "console0"}}
}

// An open request signed with the key passes once; a replay, a changed field, another key, a request outside the window and a request
// without a key all fail.
func TestCableAuth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := &cableAuth{key: StaticCableKey(testCableKey), nonces: newNonceCache(100), now: func() time.Time { return now }}
	req, err := signOpen(testCableKey, testOpen(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.check(req); err != nil {
		t.Fatalf("a signed request: %v", err)
	}
	if err := a.check(req); !errors.Is(err, errCableReplay) {
		t.Fatalf("a replayed request: %v", err)
	}
	for name, mod := range map[string]func(*wire.CableOpen){
		"another lab":         func(r *wire.CableOpen) { r.Lab = "lab2" },
		"another namespace":   func(r *wire.CableOpen) { r.Namespace = "other" },
		"another serial port": func(r *wire.CableOpen) { r.Serial.Port = "serial1" },
		"another cable uid":   func(r *wire.CableOpen) { r.UID = "x" },
	} {
		fresh, _ := signOpen(testCableKey, testOpen(), now)
		mod(&fresh)
		if err := a.check(fresh); !errors.Is(err, errCableMAC) {
			t.Errorf("%s after signing: %v", name, err)
		}
	}
	forged, _ := signOpen([]byte(strings.Repeat("x", 48)), testOpen(), now)
	if err := a.check(forged); !errors.Is(err, errCableMAC) {
		t.Fatalf("another key: %v", err)
	}
	old, _ := signOpen(testCableKey, testOpen(), now.Add(-CableWindow-time.Second))
	if err := a.check(old); !errors.Is(err, errCableTime) {
		t.Fatalf("an old request: %v", err)
	}
	future, _ := signOpen(testCableKey, testOpen(), now.Add(CableWindow+time.Second))
	if err := a.check(future); !errors.Is(err, errCableTime) {
		t.Fatalf("a request from the future: %v", err)
	}
	nokey := &cableAuth{key: StaticCableKey([]byte("short")), nonces: newNonceCache(10), now: a.now}
	fresh, _ := signOpen([]byte("short"), testOpen(), now)
	if err := nokey.check(fresh); !errors.Is(err, errCableKey) {
		t.Fatalf("a short key must not be used: %v", err)
	}
}

// The reply is bound to the nonce of the request and to the key.
func TestCableReply(t *testing.T) {
	r := signReply(testCableKey, wire.CableReply{OK: true, Nonce: "n1"})
	if err := checkReply(testCableKey, r, "n1"); err != nil {
		t.Fatal(err)
	}
	if checkReply(testCableKey, r, "n2") == nil {
		t.Fatal("a reply to another request")
	}
	r.OK = false
	if checkReply(testCableKey, r, "n1") == nil {
		t.Fatal("a changed reply")
	}
	if checkReply([]byte(strings.Repeat("z", 48)), signReply(testCableKey, wire.CableReply{Nonce: "n1"}), "n1") == nil {
		t.Fatal("another key")
	}
}

// A full nonce cache refuses rather than forget; nonces older than twice the window are forgotten.
func TestNonceCache(t *testing.T) {
	c := newNonceCache(2)
	now := time.Unix(100, 0)
	if !c.add("a", now) || !c.add("b", now) || c.add("c", now) {
		t.Fatal("capacity")
	}
	if !c.add("c", now.Add(2*CableWindow+time.Second)) {
		t.Fatal("old nonces must be forgotten")
	}
}

// The key file is read again when it changes.
func TestCableKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if _, err := NewCableKey(p).Get(); err == nil {
		t.Fatal("no file")
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("a", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := NewCableKey(p)
	got, err := k.Get()
	if err != nil || string(got) != strings.Repeat("a", 40) {
		t.Fatalf("%q %v", got, err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("b", 41)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.Get(); string(got) != strings.Repeat("b", 41) {
		t.Fatalf("a rotated key: %q", got)
	}
}
