package devicestate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

type manifestTestStore map[digest.Digest][]byte

type manifestTestReader struct{ *bytes.Reader }

func (r manifestTestReader) Close() error { return nil }
func (r manifestTestReader) Size() int64  { return r.Reader.Size() }
func (s manifestTestStore) ReaderAt(_ context.Context, d ocispec.Descriptor) (content.ReaderAt, error) {
	b, ok := s[d.Digest]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	return manifestTestReader{bytes.NewReader(b)}, nil
}

func manifestFixture(t *testing.T) (manifestTestStore, ocispec.Descriptor, ocispec.Descriptor) {
	t.Helper()
	config := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cfg := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.FromBytes(config), Size: int64(len(config))}
	m := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{}}
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	native := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw)), Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}
	compatible := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("compatible-arm-v6-not-pulled"), Platform: &ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}}
	return manifestTestStore{cfg.Digest: config, native.Digest: raw}, native, compatible
}

func manifestIndex(t *testing.T, s manifestTestStore, descriptors ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	idx := ocispec.Index{Manifests: descriptors}
	idx.SchemaVersion = 2
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	s[d.Digest] = raw
	return d
}

func loadManifestFixture(ctx context.Context, s manifestTestStore, d ocispec.Descriptor) (v1.Image, error) {
	h, err := resolveManifest(ctx, s, d, platforms.Only(ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}))
	if err != nil {
		return nil, err
	}
	hash, err := v1.NewHash(h.String())
	if err != nil {
		return nil, err
	}
	return snapshot.LoadImage(ctx, contentSource{s}, hash)
}

func TestResolveManifestPrefersNativeOverCompatibleIndexOrder(t *testing.T) {
	for _, firstNative := range []bool{false, true} {
		t.Run(map[bool]string{false: "compatible-first", true: "native-first"}[firstNative], func(t *testing.T) {
			s, native, compatible := manifestFixture(t)
			if !platforms.Only(*native.Platform).Match(*compatible.Platform) {
				t.Fatal("fixture must reproduce compatible foreign platform")
			}
			descriptors := []ocispec.Descriptor{compatible, native}
			if firstNative {
				descriptors = []ocispec.Descriptor{native, compatible}
			}
			img, err := loadManifestFixture(context.Background(), s, manifestIndex(t, s, descriptors...))
			if err != nil {
				t.Fatalf("active native manifest/config exist; foreign compatible content must not be required: %v", err)
			}
			cfg, err := img.ConfigFile()
			if err != nil || cfg.Architecture != "arm64" || cfg.OS != "linux" {
				t.Fatalf("wrong active platform/config: %#v / %v", cfg, err)
			}
		})
	}
}

func TestResolveManifestMissingPreferredContentRemainsError(t *testing.T) {
	s, native, compatible := manifestFixture(t)
	config := []byte(`{"architecture":"arm","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cfg := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.FromBytes(config), Size: int64(len(config))}
	m := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{}}
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	compatible.Digest = digest.FromBytes(raw)
	compatible.Size = int64(len(raw))
	s[compatible.Digest] = raw
	s[cfg.Digest] = config
	delete(s, native.Digest)
	_, err = loadManifestFixture(context.Background(), s, manifestIndex(t, s, compatible, native))
	if err == nil || !errors.Is(err, errdefs.ErrNotFound) {
		t.Fatalf("missing preferred content must not silently fall back: %v", err)
	}
}

func TestResolveManifestLegacyAndIncompatibleDescriptors(t *testing.T) {
	for _, name := range []string{"unspecified-first", "unspecified-only", "incompatible-first", "incompatible-only"} {
		t.Run(name, func(t *testing.T) {
			s, native, _ := manifestFixture(t)
			unknown := native
			unknown.Platform = nil
			foreign := native
			foreign.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
			descriptors := []ocispec.Descriptor{unknown, native}
			switch name {
			case "unspecified-only":
				descriptors = []ocispec.Descriptor{unknown}
			case "incompatible-first":
				descriptors = []ocispec.Descriptor{foreign, native}
			case "incompatible-only":
				descriptors = []ocispec.Descriptor{foreign}
			}
			h, err := resolveManifest(context.Background(), s, manifestIndex(t, s, descriptors...), platforms.Only(*native.Platform))
			if name == "incompatible-only" {
				if err == nil || !strings.Contains(err.Error(), "no manifest for this platform") {
					t.Fatal(err)
				}
			} else if err != nil || h != native.Digest {
				t.Fatalf("legacy/compatible selection changed: %s / %v", h, err)
			}
		})
	}
}

func TestResolveManifestPreservesMalformedAndMissingContentErrors(t *testing.T) {
	t.Run("index", func(t *testing.T) {
		s := manifestTestStore{}
		d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromString("bad-index")}
		s[d.Digest] = []byte(`{"manifests":`)
		_, err := loadManifestFixture(context.Background(), s, d)
		if err == nil || !strings.Contains(err.Error(), "parse index") {
			t.Fatal(err)
		}
	})
	t.Run("missing-index", func(t *testing.T) {
		_, err := loadManifestFixture(context.Background(), manifestTestStore{}, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromString("missing-index")})
		if !errors.Is(err, errdefs.ErrNotFound) {
			t.Fatal(err)
		}
	})
	t.Run("missing-config", func(t *testing.T) {
		s, native, _ := manifestFixture(t)
		var manifest ocispec.Manifest
		_ = json.Unmarshal(s[native.Digest], &manifest)
		delete(s, manifest.Config.Digest)
		img, err := loadManifestFixture(context.Background(), s, native)
		if err == nil {
			_, err = img.ConfigFile()
		}
		if !errors.Is(err, errdefs.ErrNotFound) {
			t.Fatal(err)
		}
	})
	t.Run("corrupt-config", func(t *testing.T) {
		s, native, _ := manifestFixture(t)
		var manifest ocispec.Manifest
		_ = json.Unmarshal(s[native.Digest], &manifest)
		s[manifest.Config.Digest] = []byte("broken")
		img, err := loadManifestFixture(context.Background(), s, native)
		if err == nil {
			_, err = img.ConfigFile()
		}
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("corrupt config must remain an error: %v", err)
		}
	})
}
