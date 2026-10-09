package devicestate

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// Metadata remains pinned to the already selected local manifest/config. Some
// containerd configurations discard packed layers after unpacking; recover only
// those manifest-listed bytes from the original repository by immutable digest.
// No tag lookup, platform reselection or content-store mutation is performed.
func loadRegistryBackedImage(ctx context.Context, cs content.Provider, manifest v1.Hash, original string, keychain authn.Keychain) (v1.Image, error) {
	src := &registryLayerSource{local: contentSource{cs}, keychain: keychain}
	img, err := snapshot.LoadImage(ctx, src, manifest)
	if err != nil {
		return nil, err
	}
	ref, err := name.ParseReference(original)
	if err != nil {
		return nil, fmt.Errorf("original image repository: %w", err)
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	src.repo = ref.Context()
	src.layers = make(map[v1.Hash]int64, len(m.Layers))
	for _, layer := range m.Layers {
		if layer.Size < 0 {
			return nil, fmt.Errorf("negative packed layer size for %s", layer.Digest)
		}
		if size, ok := src.layers[layer.Digest]; ok && size != layer.Size {
			return nil, fmt.Errorf("conflicting packed layer sizes for %s", layer.Digest)
		}
		src.layers[layer.Digest] = layer.Size
	}
	return img, nil
}

type registryLayerSource struct {
	local    snapshot.BlobSource
	repo     name.Repository
	keychain authn.Keychain
	layers   map[v1.Hash]int64
}

func (s *registryLayerSource) ReadBlob(ctx context.Context, h v1.Hash) (io.ReadCloser, int64, error) {
	rc, size, err := s.local.ReadBlob(ctx, h)
	if err == nil || !errdefs.IsNotFound(err) {
		return rc, size, err
	}
	expected, allowed := s.layers[h]
	if !allowed {
		return nil, 0, err // metadata and unlisted digests never use the fallback
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	keychain := s.keychain
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	layer, err := remote.Layer(s.repo.Digest(h.String()), remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain))
	if err != nil {
		return nil, 0, fmt.Errorf("read original packed layer %s: %w", h, err)
	}
	rc, err = layer.Compressed() // remote verifies the requested digest at EOF
	if err != nil {
		return nil, 0, fmt.Errorf("read original packed layer %s: %w", h, err)
	}
	return &packedSizeReader{ReadCloser: rc, expected: expected}, expected, nil
}

// Bound streamed bytes by the manifest descriptor as well as remote's digest
// verification. A short/long or corrupt response must abort the snapshot push.
type packedSizeReader struct {
	io.ReadCloser
	expected, read int64
	err            error
}

func (r *packedSizeReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	remaining := r.expected - r.read
	if remaining < int64(len(p)) {
		p = p[:min(int64(len(p)), remaining+1)]
	}
	n, err := r.ReadCloser.Read(p)
	r.read += int64(n)
	if r.read > r.expected || err == io.EOF && r.read != r.expected {
		r.err = fmt.Errorf("original packed layer size differs from manifest: expected %d, read %d", r.expected, r.read)
		return n, r.err
	}
	return n, err
}
