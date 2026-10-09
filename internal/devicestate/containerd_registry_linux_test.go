package devicestate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type registryTestKeychain struct{ auth authn.Authenticator }

func (k registryTestKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return k.auth, nil
}

type packedLayerFixture struct {
	local      manifestTestStore
	manifest   v1.Hash
	layer      v1.Hash
	body       []byte
	server     *httptest.Server
	ref        string
	mu         sync.Mutex
	requests   []string
	status     int
	remoteBody []byte
}

func newPackedLayerFixture(t *testing.T) *packedLayerFixture {
	t.Helper()
	body := []byte("immutable packed base layer")
	l := static.NewLayer(body, types.DockerLayer)
	img, err := mutate.AppendLayers(empty.Image, l)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := img.Digest()
	lh, _ := l.Digest()
	raw, _ := img.RawManifest()
	cfg, _ := img.RawConfigFile()
	ch, _ := img.ConfigName()
	f := &packedLayerFixture{local: manifestTestStore{digest.Digest(md.String()): raw, digest.Digest(ch.String()): cfg}, manifest: md, layer: lh, body: body, remoteBody: body, status: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		u, p, ok := r.BasicAuth()
		if !ok || u != "reader" || p != "fixture-only" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v2/original/blobs/"+lh.String() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			return
		}
		_, _ = w.Write(f.remoteBody)
	}))
	t.Cleanup(f.server.Close)
	f.ref = strings.TrimPrefix(f.server.URL, "http://") + "/original@" + md.String()
	return f
}

func packedRead(t *testing.T, f *packedLayerFixture, keychain authn.Keychain) ([]byte, error) {
	t.Helper()
	img, err := loadRegistryBackedImage(context.Background(), f.local, f.manifest, f.ref, keychain)
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestDiscardedPackedLayerRetrievedByImmutableDigest(t *testing.T) {
	f := newPackedLayerFixture(t)
	got, err := packedRead(t, f, registryTestKeychain{&authn.Basic{Username: "reader", Password: "fixture-only"}})
	if err != nil || string(got) != string(f.body) {
		t.Fatalf("unpacked active image must recover original packed bytes: %q / %v", got, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if strings.Contains(request, "/manifests/") {
			t.Fatalf("must not re-resolve mutable tag/platform: %s", request)
		}
	}
	if len(f.requests) < 2 {
		t.Fatal("no authenticated original registry read")
	}
	if _, ok := f.local[digest.Digest(f.layer.String())]; ok {
		t.Fatal("must not inject remote bytes into containerd content")
	}
}

func TestLocalPackedLayerDoesNotContactRegistry(t *testing.T) {
	f := newPackedLayerFixture(t)
	f.local[digest.Digest(f.layer.String())] = f.body
	got, err := packedRead(t, f, registryTestKeychain{authn.Anonymous})
	if err != nil || string(got) != string(f.body) {
		t.Fatal(err)
	}
	if len(f.requests) != 0 {
		t.Fatal("local content should not contact original registry")
	}
}

func TestPackedLayerRecoveryKeepsAuthDigestAndSizeErrors(t *testing.T) {
	for _, mode := range []string{"auth", "missing", "digest", "size"} {
		t.Run(mode, func(t *testing.T) {
			f := newPackedLayerFixture(t)
			keychain := registryTestKeychain{&authn.Basic{Username: "reader", Password: "fixture-only"}}
			switch mode {
			case "auth":
				keychain.auth = authn.Anonymous
			case "missing":
				f.status = http.StatusNotFound
			case "digest":
				f.remoteBody = []byte(strings.Repeat("x", len(f.body)))
			case "size":
				f.remoteBody = append(append([]byte{}, f.body...), 'x')
			}
			if _, err := packedRead(t, f, keychain); err == nil {
				t.Fatalf("%s failure must not be accepted", mode)
			}
		})
	}
}

func TestPackedRecoveryDoesNotFetchMetadataOrUnlistedDigest(t *testing.T) {
	f := newPackedLayerFixture(t)
	delete(f.local, digest.Digest(f.manifest.String()))
	_, err := loadRegistryBackedImage(context.Background(), f.local, f.manifest, f.ref, registryTestKeychain{authn.Anonymous})
	if !errors.Is(err, errdefs.ErrNotFound) {
		t.Fatalf("missing selected manifest must remain error: %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatal("must not fetch/reselect metadata")
	}
	if _, err := name.ParseReference(f.ref); err != nil {
		t.Fatal(err)
	}
}

type packedFailingStore struct {
	manifestTestStore
	layer digest.Digest
	err   error
}

func (s packedFailingStore) ReaderAt(ctx context.Context, d ocispec.Descriptor) (content.ReaderAt, error) {
	if d.Digest == s.layer {
		return nil, s.err
	}
	return s.manifestTestStore.ReaderAt(ctx, d)
}

func TestPackedRecoveryPreservesLocalErrorsAndCancellation(t *testing.T) {
	f := newPackedLayerFixture(t)
	failure := errors.New("local content I/O unavailable")
	store := packedFailingStore{f.local, digest.Digest(f.layer.String()), failure}
	img, err := loadRegistryBackedImage(context.Background(), store, f.manifest, f.ref, registryTestKeychain{authn.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := img.Layers()
	if _, err := layers[0].Compressed(); !errors.Is(err, failure) {
		t.Fatalf("local I/O error must not trigger registry fallback: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	img, err = loadRegistryBackedImage(ctx, f.local, f.manifest, f.ref, registryTestKeychain{authn.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	layers, _ = img.Layers()
	if _, err := layers[0].Compressed(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled recovery must not contact source: %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatal("non-notfound/cancel contacted original registry")
	}
}

func TestPackedRecoveryRejectsUnlistedDigestAndKeepsSizeErrorStable(t *testing.T) {
	f := newPackedLayerFixture(t)
	ref, _ := name.ParseReference(f.ref)
	src := &registryLayerSource{local: contentSource{f.local}, repo: ref.Context(), layers: map[v1.Hash]int64{f.layer: int64(len(f.body))}, keychain: registryTestKeychain{authn.Anonymous}}
	if _, _, err := src.ReadBlob(context.Background(), v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("f", 64)}); !errors.Is(err, errdefs.ErrNotFound) {
		t.Fatalf("unlisted digest did not preserve missing-content error: %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatal("unlisted digest contacted original registry")
	}
	r := &packedSizeReader{ReadCloser: io.NopCloser(strings.NewReader("long")), expected: 1}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("oversized packed bytes accepted")
	}
	if _, err := r.Read(make([]byte, 8)); err == nil {
		t.Fatal("size refusal must remain an error on another read")
	}
}
