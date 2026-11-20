package registrygate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func hexd(i int) string { return fmt.Sprintf("sha256:%064x", i) }

// Review finding 8: the grant table is bounded in every dimension and says so when it refuses.
func TestGrantTableIsBoundedAndRefusesLoudly(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g := &Grants{Now: func() time.Time { return now }}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, "sha256:short"); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("a malformed digest: %v", err)
	}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, strings.Repeat("a", 64)); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("a digest without the algorithm: %v", err)
	}
	if g.Len() != 0 {
		t.Fatal("a refused grant leaves nothing")
	}
	var many []string
	for i := 0; i <= maxDigestsPerGrant; i++ {
		many = append(many, hexd(i))
	}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, many...); !errors.Is(err, ErrTooManyBlobs) {
		t.Fatalf("too many digests for one grant: %v", err)
	}
	if g.Len() != 0 {
		t.Fatal("a grant that was refused is not half-made")
	}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, many[:maxDigestsPerGrant]...); err != nil {
		t.Fatal(err)
	}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, hexd(999999)); !errors.Is(err, ErrTooManyBlobs) {
		t.Fatalf("growing a full grant: %v", err)
	}
	// one node cannot fill the table: its own cap, and another node is not affected
	for i := 0; i < maxGrantsPerNode-1; i++ {
		if err := g.Add("n1", "acme", fmt.Sprintf("r/%d", i), Creds{}, hexd(1)); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if err := g.Add("n1", "acme", "r/over", Creds{}, hexd(1)); !errors.Is(err, ErrGrantsFull) {
		t.Fatalf("the node's cap: %v", err)
	}
	if err := g.Add("n2", "acme", "r/over", Creds{}, hexd(1)); err != nil {
		t.Fatalf("another node: %v", err)
	}
	// lapsed grants make room, found without scanning the whole table
	now = now.Add(3 * time.Minute)
	if err := g.Add("n1", "acme", "r/new", Creds{}, hexd(1)); err != nil {
		t.Fatalf("the lapsed ones make room: %v", err)
	}
	g.Sweep()
	if g.Len() != 1 {
		t.Fatalf("a sweep drops what lapsed: %d left", g.Len())
	}
}

// A manifest whose grant cannot be made is refused, not served with blobs nobody can then fetch.
func TestAManifestThatCannotBeGrantedIsRefused(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
	for i := 0; i < maxGrantsPerNode; i++ {
		if err := n.cache.Grants.Add("node-a", "acme", fmt.Sprintf("filler/%d", i), Creds{}, hexd(1)); err != nil {
			t.Fatal(err)
		}
	}
	code, body, hdr := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil)
	if code != 429 || hdr.Get("Retry-After") == "" || !strings.Contains(string(body), "CIB_GRANTS_FULL") {
		t.Fatalf("%d %s %v", code, body, hdr)
	}
	if hdr.Get("Docker-Content-Digest") != "" {
		t.Fatalf("the held manifest's headers must not leak: %v", hdr)
	}
}
