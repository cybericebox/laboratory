package registrygate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func hexd(i int) string { return fmt.Sprintf("sha256:%064x", i) }

// Review finding 8 / 2.4.6a: the grant table is bounded in every dimension; a node's oldest grant gives way; a manifest too big is refused.
func TestGrantTableIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g := &Grants{Now: func() time.Time { return now }}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, "", hexd(1), []string{"sha256:short"}); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("a malformed digest: %v", err)
	}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, "", strings.Repeat("a", 64), nil); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("a root without the algorithm: %v", err)
	}
	var many []string
	for i := 0; i < DefaultGrantMaxDigests; i++ {
		many = append(many, hexd(100+i))
	}
	if err := g.Add("n1", "acme", "docker.io/x/y", Creds{}, "", hexd(1), many); !errors.Is(err, ErrTooManyBlobs) {
		t.Fatalf("a manifest that lists more than a grant holds: %v", err)
	}
	if g.Len() != 0 {
		t.Fatal("a refused grant leaves nothing")
	}
	// the oldest grant of a node gives way to a new one at the cap; another node is unaffected
	g.PerNode = 3
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		if err := g.Add("n1", "acme", fmt.Sprintf("r/%d", i), Creds{}, "", hexd(1), nil); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second)
	if err := g.Add("n1", "acme", "r/new", Creds{}, "", hexd(1), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Has("n1", "acme", "r/0", hexd(1), true); ok {
		t.Fatal("the oldest grant of the node must be dropped")
	}
	for _, r := range []string{"r/1", "r/2", "r/new"} {
		if _, ok := g.Has("n1", "acme", r, hexd(1), true); !ok {
			t.Fatalf("%s must stay", r)
		}
	}
	if err := g.Add("n2", "acme", "r/0", Creds{}, "", hexd(1), nil); err != nil {
		t.Fatal(err)
	}
	// lapsed grants are swept, never scanned per request
	now = now.Add(3 * time.Minute)
	g.Sweep()
	if g.Len() != 0 {
		t.Fatalf("a sweep drops what lapsed: %d left", g.Len())
	}
}

// Review 2, item 1: an answer on a stale decision only renews the expiry of what is already granted: no new root, child, tag or credential,
// and no grant. A child manifest served under a live grant may add its listing to that grant, but nothing else.
func TestRenewAndExpandNeverWidenWhatWasProven(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g := &Grants{Now: func() time.Time { return now }}
	if g.Renew("n", "acme", "r/x", hexd(1)) || g.Expand("n", "acme", "r/x", hexd(1), []string{hexd(2)}) || g.Len() != 0 {
		t.Fatal("no grant: nothing is made")
	}
	if err := g.Add("n", "acme", "r/x", Creds{"alice", "pw"}, "", hexd(1), []string{hexd(2)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute + 30*time.Second)
	if g.Renew("n", "acme", "r/x", hexd(9)) {
		t.Fatal("a root the grant does not hold is not renewed")
	}
	if !g.Renew("n", "acme", "r/x", hexd(1)) {
		t.Fatal("a held root is renewed")
	}
	now = now.Add(time.Minute) // past the original expiry, inside the renewed one
	cr, ok := g.Has("n", "acme", "r/x", hexd(2), true)
	if !ok || cr != (Creds{"alice", "pw"}) {
		t.Fatalf("the renewed grant keeps what it had: %v %v", ok, cr)
	}
	if _, ok := g.Has("n", "acme", "r/x", hexd(3), true); ok {
		t.Fatal("a stale renewal added a digest")
	}
	if g.Expand("n", "acme", "r/x", hexd(9), []string{hexd(3)}) {
		t.Fatal("a root the grant does not hold cannot be expanded")
	}
	if !g.Expand("n", "acme", "r/x", hexd(2), []string{hexd(3)}) {
		t.Fatal("a child under the grant adds its listing")
	}
	if !g.Covers("n", "acme", "r/x", hexd(1), hexd(3)) {
		t.Fatal("and it is reachable from the pod's reference")
	}
	now = now.Add(5 * time.Minute) // lapsed
	if g.Renew("n", "acme", "r/x", hexd(1)) || g.Expand("n", "acme", "r/x", hexd(2), []string{hexd(4)}) {
		t.Fatal("a lapsed grant is not brought back")
	}
}

// Review 2, item 6: a full grant gives way to its oldest root, never wiped; the error names the limit.
func TestAFullGrantEvictsItsOldestRootAndTheErrorNamesTheLimit(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g := &Grants{Now: func() time.Time { return now }, MaxDigests: 6}
	add := func(root int, kids ...int) error {
		var ks []string
		for _, k := range kids {
			ks = append(ks, hexd(k))
		}
		return g.Add("n", "acme", "r/x", Creds{}, "", hexd(root), ks)
	}
	if err := add(1, 11, 12); err != nil { // 3 digests
		t.Fatal(err)
	}
	if err := add(2, 21, 22); err != nil { // 6
		t.Fatal(err)
	}
	if err := add(3, 31, 32); err != nil { // would be 9: the oldest root (1) gives way, not the whole grant
		t.Fatal(err)
	}
	if _, ok := g.Has("n", "acme", "r/x", hexd(11), true); ok {
		t.Fatal("the oldest root's digests are gone")
	}
	for _, d := range []int{2, 21, 22, 3, 31, 32} {
		if _, ok := g.Has("n", "acme", "r/x", hexd(d), true); !ok {
			t.Fatalf("digest %d of a newer root must stay: other pulls of the repository are still in flight", d)
		}
	}
	err := add(4, 41, 42, 43, 44, 45, 46) // 7 digests: more than a grant holds, whatever is evicted
	if !errors.Is(err, ErrTooManyBlobs) || !strings.Contains(err.Error(), "GATE_GRANT_MAX_DIGESTS") || !strings.Contains(err.Error(), "7") {
		t.Fatalf("the message must say which limit was hit: %v", err)
	}
}

// Covers is about one reference, not about the repository: a digest granted for another image of the same repository is not covered.
func TestCoversFollowsOneReference(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g := &Grants{Now: func() time.Time { return now }}
	idx, child, layer, other, otherLayer := hexd(1), hexd(2), hexd(3), hexd(4), hexd(5)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(g.Add("n", "acme", "r/x", Creds{}, "", idx, []string{child}))
	must(g.Add("n", "acme", "r/x", Creds{}, "", child, []string{layer}))
	must(g.Add("n", "acme", "r/x", Creds{}, "stable", other, []string{otherLayer}))
	for _, c := range []struct {
		ref, d string
		want   bool
	}{
		{idx, idx, true}, {idx, child, true}, {idx, layer, true},
		{idx, other, false}, {idx, otherLayer, false},
		{"stable", otherLayer, true}, {"stable", layer, false},
		{"unknown-tag", layer, false},
	} {
		if got := g.Covers("n", "acme", "r/x", c.ref, c.d); got != c.want {
			t.Errorf("ref %.8s digest %.12s: %v, want %v", c.ref, c.d, got, c.want)
		}
	}
	if g.Covers("other-node", "acme", "r/x", idx, layer) || g.Covers("n", "other", "r/x", idx, layer) {
		t.Fatal("another node or segment has no such grant")
	}
}

// 2.4.6a: a manifest that is too big, or lists more digests than a grant holds, is 403 CIB_MANIFEST_TOO_BIG, never served half-granted.
func TestAManifestTooBigIsRefused(t *testing.T) {
	for name, set := range map[string]func(n *nodeRig){
		"bytes":   func(n *nodeRig) { n.cache.ManifestMaxBytes = 10 },
		"digests": func(n *nodeRig) { n.cache.Grants.MaxDigests = 1 },
	} {
		n := newNodeRig(t)
		n.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
		set(n)
		code, body, hdr := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil)
		if code != 403 || !strings.Contains(string(body), "CIB_MANIFEST_TOO_BIG") {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		if hdr.Get("Docker-Content-Digest") != "" {
			t.Fatalf("%s: the held manifest's headers leaked: %v", name, hdr)
		}
	}
}
