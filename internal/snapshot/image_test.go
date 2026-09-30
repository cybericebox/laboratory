package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/validate"
)

// memSource serves the blobs of an image, like the containerd content store
// holds them for an image the kubelet pulled.
type memSource map[v1.Hash][]byte

func sourceOf(t *testing.T, img v1.Image) (memSource, v1.Hash) {
	t.Helper()
	src := memSource{}
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := img.RawManifest()
	md, _ := img.Digest()
	src[md] = raw
	cfg, _ := img.RawConfigFile()
	src[m.Config.Digest] = cfg
	layers, _ := img.Layers()
	for _, l := range layers {
		d, _ := l.Digest()
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		src[d] = b
	}
	return src, md
}

func (m memSource) ReadBlob(_ context.Context, h v1.Hash) (io.ReadCloser, int64, error) {
	b, ok := m[h]
	if !ok {
		return nil, 0, errors.New("blob not found " + h.String())
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func writeTar(t *testing.T, dir, name string, ents ...ent) (string, int64) {
	t.Helper()
	b := mkTar(t, ents...)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var n int64
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		n += h.Size
	}
	return p, n
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return &Registry{Host: strings.TrimPrefix(srv.URL, "http://")}
}

// snapshotOnce mimics the node-agent: load the image the container runs from
// out of the local store, build the next snapshot, push it, and return the
// pushed image as the next run's starting point.
func snapshotOnce(t *testing.T, reg *Registry, repo string, run v1.Image, pol Policy, tarName string, ents ...ent) (v1.Image, error) {
	t.Helper()
	dir := t.TempDir()
	src, md := sourceOf(t, run)
	loaded, err := LoadImage(context.Background(), src, md)
	if err != nil {
		t.Fatal(err)
	}
	p, n := writeTar(t, dir, tarName, ents...)
	next, chain, err := Build(loaded, p, n, pol, dir)
	if err != nil {
		return nil, err
	}
	_, _, err = reg.Push(context.Background(), repo, next, chain.Base, "")
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := reg.Image(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return pushed, nil
}

func TestSnapshotChainPushAndSquash(t *testing.T) {
	reg := testRegistry(t)
	base, err := random.Image(2048, 3)
	if err != nil {
		t.Fatal(err)
	}
	base, err = mutate.Config(base, v1.Config{Entrypoint: []string{"/srv/run"}, Env: []string{"A=1"}, WorkingDir: "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	pol := NewPolicy(0, nil, 1<<20, 3)
	repo := Repo("ns", "lab", "web")

	s1, err := snapshotOnce(t, reg, repo, base, pol, "1.tar", ent{name: "data/a", body: "run1"}, ent{name: "data/keep", body: "k"})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := ChainOf(s1)
	if err != nil || chain.Base != 3 || chain.Layers() != 1 || chain.Bytes() != 5 {
		t.Fatalf("after first snapshot: %+v %v", chain, err)
	}
	cfg, _ := s1.ConfigFile()
	if cfg.Config.Entrypoint[0] != "/srv/run" || cfg.Config.WorkingDir != "/srv" {
		t.Fatalf("config not preserved: %+v", cfg.Config)
	}
	if err := validate.Image(s1); err != nil {
		t.Fatal(err)
	}

	s2, err := snapshotOnce(t, reg, repo, s1, pol, "2.tar", ent{name: "data/a", body: "run2!"}, ent{name: "data/.wh.keep"})
	if err != nil {
		t.Fatal(err)
	}
	if chain, _ = ChainOf(s2); chain.Layers() != 2 {
		t.Fatalf("want 2 snapshot layers, got %+v", chain)
	}
	s3, err := snapshotOnce(t, reg, repo, s2, pol, "3.tar", ent{name: "data/b", body: "bb"})
	if err != nil {
		t.Fatal(err)
	}
	if chain, _ = ChainOf(s3); chain.Layers() != 3 {
		t.Fatalf("want 3 snapshot layers, got %+v", chain)
	}
	// The fourth snapshot exceeds maxLayers=3 and squashes the chain.
	s4, err := snapshotOnce(t, reg, repo, s3, pol, "4.tar", ent{name: "data/c", body: "c"})
	if err != nil {
		t.Fatal(err)
	}
	chain, err = ChainOf(s4)
	if err != nil {
		t.Fatal(err)
	}
	if chain.Base != 3 || chain.Layers() != 1 {
		t.Fatalf("squash must leave one snapshot layer on the 3 base layers: %+v", chain)
	}
	if want := int64(len("run2!") + len("bb") + len("c")); chain.Bytes() != want {
		t.Fatalf("squashed size %d want %d", chain.Bytes(), want)
	}
	if err := validate.Image(s4); err != nil {
		t.Fatal(err)
	}
	layers, _ := s4.Layers()
	rc, _ := layers[3].Uncompressed()
	files, markers, _ := mergedView(t, mustRead(t, rc))
	if files["data/a"] != "run2!" || files["data/b"] != "bb" || files["data/c"] != "c" || len(files) != 3 {
		t.Fatalf("squashed files %v", files)
	}
	if len(markers) != 1 || markers[0] != "data/.wh.keep" {
		t.Fatalf("squashed markers %v", markers)
	}
	cfg4, _ := s4.ConfigFile()
	if cfg4.Config.Entrypoint[0] != "/srv/run" || len(cfg4.RootFS.DiffIDs) != 4 {
		t.Fatalf("squashed config: %+v", cfg4)
	}
}

func mustRead(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuildRefusesOverQuotaAndKeepsImage(t *testing.T) {
	base, _ := random.Image(512, 1)
	dir := t.TempDir()
	src, md := sourceOf(t, base)
	loaded, err := LoadImage(context.Background(), src, md)
	if err != nil {
		t.Fatal(err)
	}
	p, n := writeTar(t, dir, "big.tar", ent{name: "blob", body: strings.Repeat("x", 100)})
	_, _, err = Build(loaded, p, n, NewPolicy(0, nil, 50, 10), dir)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("want ErrQuota, got %v", err)
	}
}

func TestBaseLayersSharedAcrossDeviceRepos(t *testing.T) {
	reg := testRegistry(t)
	base, _ := random.Image(1024, 2)
	pol := NewPolicy(0, nil, 0, 0)
	for _, dev := range []string{"a", "b"} {
		if _, err := snapshotOnce(t, reg, Repo("ns", "lab", dev), base, pol, "x.tar", ent{name: "f", body: "1"}); err != nil {
			t.Fatal(err)
		}
	}
	repos, err := reg.Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"lab/ns/lab/a": true, "lab/ns/lab/b": true}
	for _, r := range repos {
		delete(want, r)
	}
	if len(want) != 0 {
		t.Fatalf("missing repos %v in %v", want, repos)
	}
	// The base layers sit in the shared repository, uploaded once.
	layers, _ := base.Layers()
	d, _ := layers[0].Digest()
	baseRepo, _ := reg.repo(BaseRepo)
	if !reg.hasBlob(context.Background(), baseRepo, d) {
		t.Fatal("base layer missing from the shared base repository")
	}
}

func TestDeleteRepo(t *testing.T) {
	reg := testRegistry(t)
	base, _ := random.Image(512, 1)
	repo := Repo("ns", "lab", "web")
	pushed, err := snapshotOnce(t, reg, repo, base, NewPolicy(0, nil, 0, 0), "x.tar", ent{name: "f", body: "1"})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := pushed.Digest()
	target, _ := reg.repo(repo)
	if _, err := remote.Head(target.Digest(digest.String()), reg.opts(context.Background())...); err != nil {
		t.Fatal(err)
	}
	if err := reg.DeleteRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(target.Digest(digest.String()), reg.opts(context.Background())...); err == nil {
		t.Fatal("snapshot manifest must be gone")
	}
	// Deleting a repository that does not exist is not an error.
	if err := reg.DeleteRepo(context.Background(), "lab/none/none/none"); err != nil {
		t.Fatal(err)
	}
}
