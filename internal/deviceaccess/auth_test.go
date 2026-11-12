package deviceaccess

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

func signed(t *testing.T, key []byte, at time.Time) wire.OpenRequest {
	t.Helper()
	r := wire.OpenRequest{Kind: wire.KindConsole, Namespace: "ns", Lab: "lab", Device: "dev", Command: []string{"/bin/sh"}, User: "kali", Cols: 80, Rows: 24, Client: "c", Group: "g"}
	if err := wire.Sign(key, &r, at); err != nil {
		t.Fatal(err)
	}
	return r
}

func authAt(now *time.Time, keys ...[]byte) *Auth {
	a := NewAuth(func() [][]byte { return keys })
	a.Now = func() time.Time { return *now }
	return a
}

func TestAuthCheck(t *testing.T) {
	now := time.Now()
	k1, k2 := bytes.Repeat([]byte("1"), 32), bytes.Repeat([]byte("2"), 32)
	a := authAt(&now, k1)

	if err := a.Check(signed(t, k1, now)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := a.Check(signed(t, k2, now)); err == nil {
		t.Error("a wrong key was accepted")
	}
	unsigned := signed(t, k1, now)
	unsigned.Sig = ""
	if err := a.Check(unsigned); err == nil {
		t.Error("a missing signature was accepted")
	}
	garbage := signed(t, k1, now)
	garbage.Sig = "zz"
	if err := a.Check(garbage); err == nil {
		t.Error("a garbled signature was accepted")
	}
	noNonce := signed(t, k1, now)
	noNonce.Nonce = ""
	if err := a.Check(noNonce); err == nil {
		t.Error("a request without a nonce was accepted")
	}
	for _, d := range []time.Duration{-31 * time.Second, 31 * time.Second, -time.Hour} {
		if err := a.Check(signed(t, k1, now.Add(d))); err != errStale {
			t.Errorf("timestamp %v: %v", d, err)
		}
	}
	if err := a.Check(signed(t, k1, now.Add(-29*time.Second))); err != nil {
		t.Errorf("a timestamp inside the window: %v", err)
	}
}

func TestAuthRefusesAReplayedNonceUntilItLeavesTheWindow(t *testing.T) {
	now := time.Now()
	k := bytes.Repeat([]byte("1"), 32)
	a := authAt(&now, k)
	r := signed(t, k, now)
	if err := a.Check(r); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(r); err != errReplay {
		t.Fatalf("replay: %v", err)
	}
	// the same request later is stale anyway, and the cache forgets it
	now = now.Add(2 * time.Minute)
	if err := a.Check(r); err != errStale {
		t.Fatalf("late replay: %v", err)
	}
	if len(a.seen) != 1 {
		// the old entry is dropped when the next valid request arrives
		if err := a.Check(signed(t, k, now)); err != nil {
			t.Fatal(err)
		}
		if len(a.seen) != 1 {
			t.Fatalf("the cache kept %d nonces", len(a.seen))
		}
	}
}

func TestAuthAcceptsThePreviousKeyAndOnlyTheListedOnes(t *testing.T) {
	now := time.Now()
	cur, prev, other := bytes.Repeat([]byte("c"), 32), bytes.Repeat([]byte("p"), 32), bytes.Repeat([]byte("o"), 32)
	a := authAt(&now, cur, prev)
	if err := a.Check(signed(t, prev, now)); err != nil {
		t.Errorf("the previous key: %v", err)
	}
	if err := a.Check(signed(t, cur, now)); err != nil {
		t.Errorf("the current key: %v", err)
	}
	if err := a.Check(signed(t, other, now)); err == nil {
		t.Error("an unlisted key was accepted")
	}
}

// Every field is signed: changing any of them after signing is refused.
func TestAuthRefusesAModifiedRequest(t *testing.T) {
	now := time.Now()
	k := bytes.Repeat([]byte("1"), 32)
	mods := map[string]func(*wire.OpenRequest){
		"kind":      func(r *wire.OpenRequest) { r.Kind = "other" },
		"namespace": func(r *wire.OpenRequest) { r.Namespace = "other" },
		"lab":       func(r *wire.OpenRequest) { r.Lab = "other" },
		"device":    func(r *wire.OpenRequest) { r.Device = "other" },
		"command":   func(r *wire.OpenRequest) { r.Command = []string{"/bin/bash"} },
		"command 2": func(r *wire.OpenRequest) { r.Command = append(r.Command, "-c") },
		"user":      func(r *wire.OpenRequest) { r.User = "root" },
		"cols":      func(r *wire.OpenRequest) { r.Cols++ },
		"rows":      func(r *wire.OpenRequest) { r.Rows++ },
		"client":    func(r *wire.OpenRequest) { r.Client = "other" },
		"group":     func(r *wire.OpenRequest) { r.Group = "other" },
		"time":      func(r *wire.OpenRequest) { r.Time++ },
		"nonce":     func(r *wire.OpenRequest) { r.Nonce = "00" + r.Nonce[2:] },
	}
	for name, mod := range mods {
		a := authAt(&now, k)
		r := signed(t, k, now)
		mod(&r)
		if err := a.Check(r); err != errUnsigned {
			t.Errorf("%s changed after signing: %v", name, err)
		}
	}
	// the length prefix keeps two different splits of the same text apart
	a1 := wire.OpenRequest{Lab: "ab", Device: "c"}
	a2 := wire.OpenRequest{Lab: "a", Device: "bc"}
	if bytes.Equal(wire.Canonical(a1), wire.Canonical(a2)) {
		t.Error("two different requests have the same canonical form")
	}
}

func TestAuthWithoutKeysRefuses(t *testing.T) {
	now := time.Now()
	k := bytes.Repeat([]byte("1"), 32)
	if err := authAt(&now).Check(signed(t, k, now)); err == nil {
		t.Error("accepted with no key")
	}
	var nilAuth *Auth
	if err := nilAuth.Check(signed(t, k, now)); err == nil {
		t.Error("a nil Auth accepted")
	}
}

func TestAuthCacheIsBoundedAndFailsClosed(t *testing.T) {
	now := time.Now()
	k := bytes.Repeat([]byte("1"), 32)
	a := authAt(&now, k)
	a.seen = make(map[string]time.Time, maxNonces)
	for i := 0; i < maxNonces; i++ {
		a.seen[strconv.Itoa(i)] = now
	}
	if err := a.Check(signed(t, k, now)); err != errFull {
		t.Fatalf("%v", err)
	}
}

func TestKeyFiles(t *testing.T) {
	dir := t.TempDir()
	cur, prev := dir+"/current", dir+"/accept"
	write := func(p, v string) {
		if err := writeFile(p, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wire.NewKeyFiles(time.Hour, cur, prev); err == nil {
		t.Fatal("started without a key")
	}
	write(cur, "short")
	if _, err := wire.NewKeyFiles(time.Hour, cur, prev); err == nil {
		t.Fatal("started with a short key")
	}
	write(cur, string(bytes.Repeat([]byte("a"), 40))+"\n")
	k, err := wire.NewKeyFiles(10*time.Millisecond, cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Accepted()) != 1 || len(k.Signing()) != 40 {
		t.Fatalf("keys: %d, signing %d bytes (the newline is not part of the key)", len(k.Accepted()), len(k.Signing()))
	}
	// a second key appears (a rotation), and a broken update keeps what was held
	write(prev, string(bytes.Repeat([]byte("b"), 40)))
	time.Sleep(30 * time.Millisecond)
	if len(k.Accepted()) != 2 {
		t.Fatalf("the accepted key was not picked up: %d", len(k.Accepted()))
	}
	write(prev, "x")
	time.Sleep(30 * time.Millisecond)
	if len(k.Accepted()) != 2 {
		t.Fatalf("a bad file replaced the keys: %d", len(k.Accepted()))
	}
}
