package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// R-5: a new snapshot removes the manifest it replaced and the blobs only that one used, at once.
func TestSupersedeDeletesTheReplacedManifestAndItsOwnBlobs(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	repo := Repo("ns", "lab", "web")
	base, _ := random.Image(512, 1)

	first, err := snapshotOnce(t, reg, repo, base, NewPolicy(0, nil, 0, 0), "a.tar", ent{name: "f", body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	firstRef := reg.Ref(repo, mustDigest(t, first))
	second, err := snapshotOnce(t, reg, repo, first, NewPolicy(0, nil, 0, 1), "b.tar", ent{name: "g", body: "two"})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := reg.repo(repo)
	firstLayers, _ := first.Layers()
	secondLayers, _ := second.Layers()
	inSecond := map[string]bool{}
	for _, l := range secondLayers {
		d, _ := l.Digest()
		inSecond[d.String()] = true
	}
	var onlyFirst []string
	for _, l := range firstLayers {
		if d, _ := l.Digest(); !inSecond[d.String()] {
			onlyFirst = append(onlyFirst, d.String())
		}
	}

	if len(onlyFirst) == 0 {
		t.Fatal("the test needs a layer only the first snapshot has")
	}
	if err := reg.Supersede(ctx, repo, firstRef, second); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(target.Digest(mustDigest(t, first).String()), reg.opts(ctx)...); err == nil {
		t.Fatal("the superseded manifest must be gone")
	}
	if _, err := remote.Head(target.Digest(mustDigest(t, second).String()), reg.opts(ctx)...); err != nil {
		t.Fatalf("the current manifest must stay: %v", err)
	}
	for _, l := range secondLayers {
		d, _ := l.Digest()
		if !reg.HasBlobIn(ctx, repo, d) {
			t.Fatalf("a layer the current snapshot uses was deleted: %s", d)
		}
	}
	for _, d := range onlyFirst {
		h, _ := v1Hash(d)
		if reg.HasBlobIn(ctx, repo, h) {
			t.Errorf("a blob only the superseded snapshot used must be gone: %s", d)
		}
	}
	// the same call again, a reference without a digest (the base image), and the current one are all harmless
	if err := reg.Supersede(ctx, repo, firstRef, second); err != nil {
		t.Fatal(err)
	}
	if err := reg.Supersede(ctx, repo, "docker.io/library/app:1", second); err != nil {
		t.Fatal(err)
	}
	if err := reg.Supersede(ctx, repo, reg.Ref(repo, mustDigest(t, second)), second); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(target.Digest(mustDigest(t, second).String()), reg.opts(ctx)...); err != nil {
		t.Fatalf("superseding the current manifest by itself must do nothing: %v", err)
	}
}

// The registry says what a retired lab's repository still takes from whom, and what the volume holds.
func TestRetainedBytesAndUsage(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	base, _ := random.Image(2048, 1)
	for repo, tenant := range map[string]string{Repo("ns", "gone", "web"): "acme", Repo("ns", "alive", "web"): "acme", Repo("ns", "other", "web"): "globex"} {
		img := Annotated(base, tenant, 700)
		if _, _, err := reg.Push(ctx, repo, img, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	live := map[string]bool{Repo("ns", "alive", "web"): true}
	if got, err := reg.RetainedBytes(ctx, "acme", live); err != nil || got != 700 {
		t.Fatalf("acme: %d %v (only the repository of the lab that is gone counts)", got, err)
	}
	if got, _ := reg.RetainedBytes(ctx, "globex", live); got != 700 {
		t.Fatalf("globex: %d", got)
	}
	if got, _ := reg.RetainedBytes(ctx, "nobody", live); got != 0 {
		t.Fatalf("nobody: %d", got)
	}
	// the same base layer in three repositories is stored once
	one, err := reg.Usage(ctx)
	if err != nil || one < 2048 || one > 3*2048 {
		t.Fatalf("usage %d %v: one layer of 2048 bytes plus config and manifests, counted once", one, err)
	}
}

func TestCapacityRefusesNearTheLine(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	img, _ := random.Image(4096, 1)
	if _, _, err := reg.Push(ctx, Repo("ns", "l", "d"), img, 0, ""); err != nil {
		t.Fatal(err)
	}
	used, _ := reg.Usage(ctx)
	roomy := &Capacity{Registry: reg, Total: used * 100, ReserveFraction: 0.1}
	if err := roomy.Check(ctx, 1000); err != nil {
		t.Fatalf("plenty of room: %v", err)
	}
	tight := &Capacity{Registry: reg, Total: used + 100, ReserveFraction: 0.1}
	if err := tight.Check(ctx, 1000); !errors.Is(err, ErrRegistryFull) {
		t.Fatalf("a push past the line must be refused: %v", err)
	}
	if err := (&Capacity{Registry: reg}).Check(ctx, 1<<40); err != nil {
		t.Fatalf("no capacity configured, no refusal: %v", err)
	}
	// what a push adds is counted until the next measurement: two pushes that fit one by one do not fit together
	two := &Capacity{Registry: reg, Total: used + 1500, ReserveFraction: 0}
	if err := two.Check(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if err := two.Check(ctx, 1000); !errors.Is(err, ErrRegistryFull) {
		t.Fatalf("the second must not fit: %v", err)
	}
}

func TestAnnotatedCarriesTenantAndState(t *testing.T) {
	img, _ := random.Image(64, 1)
	m, err := Annotated(img, "acme", 42).Manifest()
	if err != nil || m.Annotations[AnnotationTenant] != "acme" || m.Annotations[AnnotationStateBytes] != "42" {
		t.Fatalf("%+v %v", m, err)
	}
}

func mustDigest(t *testing.T, img v1.Image) v1.Hash {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func v1Hash(s string) (v1.Hash, error) { return v1.NewHash(s) }

// A sparse file counts by its apparent size, which is in its header: the layer is refused before the zeros are read.
func TestFilterLayerStopsAtTheQuotaByApparentSize(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Size: 1 << 40, Mode: 0o644})
	// no body follows: reading it would fail, and the filter must not try
	pol := NewPolicy(0, nil, 1<<20, 0)
	pol.MaxFileSize = 1 << 50
	_, err := FilterLayer(&buf, io.Discard, pol)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("want ErrQuota, got %v", err)
	}
}
