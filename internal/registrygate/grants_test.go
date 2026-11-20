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
	if _, ok := g.Has("n1", "acme", "r/0", hexd(1)); ok {
		t.Fatal("the oldest grant of the node must be dropped")
	}
	for _, r := range []string{"r/1", "r/2", "r/new"} {
		if _, ok := g.Has("n1", "acme", r, hexd(1)); !ok {
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

// A grant made from a stale decision is never created, only renewed.
func TestExtendNeverCreatesAGrant(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g := &Grants{Now: func() time.Time { return now }}
	if g.Extend("n", "acme", "r/x", Creds{}, "", hexd(1), nil) || g.Len() != 0 {
		t.Fatal("no grant, none made")
	}
	if err := g.Add("n", "acme", "r/x", Creds{}, "", hexd(1), []string{hexd(2)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if !g.Extend("n", "acme", "r/x", Creds{}, "", hexd(1), []string{hexd(3)}) {
		t.Fatal("a live grant is renewed")
	}
	if _, ok := g.Has("n", "acme", "r/x", hexd(3)); !ok {
		t.Fatal("and carries the new digest")
	}
	now = now.Add(5 * time.Minute) // lapsed
	if g.Extend("n", "acme", "r/x", Creds{}, "", hexd(1), nil) {
		t.Fatal("a lapsed grant is not brought back")
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
