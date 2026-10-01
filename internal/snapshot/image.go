package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// AnnotationLayerSize marks a manifest layer as a snapshot layer and records
// its uncompressed size. Layers without it belong to the base image.
const AnnotationLayerSize = "com.cybericebox.state.size"

// historyCreatedBy marks the history entries of snapshot layers.
const historyCreatedBy = "cybericebox device state"

// BlobSource reads blobs that are already on the node (the containerd content
// store), so building a snapshot never pulls from an external registry.
type BlobSource interface {
	// ReadBlob returns the blob and its size.
	ReadBlob(ctx context.Context, digest v1.Hash) (io.ReadCloser, int64, error)
}

// LoadImage exposes an image whose single-platform manifest (and everything it
// references) is present in src as a v1.Image.
func LoadImage(ctx context.Context, src BlobSource, manifest v1.Hash) (v1.Image, error) {
	raw, err := readAll(ctx, src, manifest)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifest, err)
	}
	m, err := v1.ParseManifest(bytesReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifest, err)
	}
	cfg, err := readAll(ctx, src, m.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", m.Config.Digest, err)
	}
	return partial.CompressedToImage(&storeImage{ctx: ctx, src: src, raw: raw, m: m, cfg: cfg})
}

type storeImage struct {
	ctx context.Context
	src BlobSource
	raw []byte
	m   *v1.Manifest
	cfg []byte
}

func (s *storeImage) RawConfigFile() ([]byte, error) { return s.cfg, nil }
func (s *storeImage) RawManifest() ([]byte, error)   { return s.raw, nil }
func (s *storeImage) MediaType() (types.MediaType, error) {
	return s.m.MediaType, nil
}

func (s *storeImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	for _, l := range s.m.Layers {
		if l.Digest == h {
			return &storeLayer{ctx: s.ctx, src: s.src, desc: l}, nil
		}
	}
	return nil, fmt.Errorf("layer %s not in manifest", h)
}

type storeLayer struct {
	ctx  context.Context
	src  BlobSource
	desc v1.Descriptor
}

func (l *storeLayer) Digest() (v1.Hash, error)            { return l.desc.Digest, nil }
func (l *storeLayer) Size() (int64, error)                { return l.desc.Size, nil }
func (l *storeLayer) MediaType() (types.MediaType, error) { return l.desc.MediaType, nil }
func (l *storeLayer) Compressed() (io.ReadCloser, error) {
	rc, _, err := l.src.ReadBlob(l.ctx, l.desc.Digest)
	return rc, err
}

func readAll(ctx context.Context, src BlobSource, h v1.Hash) ([]byte, error) {
	rc, _, err := src.ReadBlob(ctx, h)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Chain describes the snapshot layers of an image.
type Chain struct {
	// Base is the number of leading layers that belong to the base image.
	Base int
	// Sizes are the uncompressed sizes of the snapshot layers, oldest first.
	Sizes []int64
}

// Layers is the number of snapshot layers.
func (c Chain) Layers() int { return len(c.Sizes) }

// Bytes is the total uncompressed size of the snapshot layers.
func (c Chain) Bytes() int64 {
	var n int64
	for _, s := range c.Sizes {
		n += s
	}
	return n
}

// ChainOf reads the snapshot chain of an image from its manifest annotations.
// Snapshot layers are always the trailing ones.
func ChainOf(img v1.Image) (Chain, error) {
	m, err := img.Manifest()
	if err != nil {
		return Chain{}, err
	}
	var c Chain
	c.Base = len(m.Layers)
	for i, l := range m.Layers {
		raw, ok := l.Annotations[AnnotationLayerSize]
		if !ok {
			if len(c.Sizes) > 0 {
				return Chain{}, fmt.Errorf("layer %d lacks %s after a snapshot layer", i, AnnotationLayerSize)
			}
			continue
		}
		if len(c.Sizes) == 0 {
			c.Base = i
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return Chain{}, fmt.Errorf("layer %d: bad %s %q", i, AnnotationLayerSize, raw)
		}
		c.Sizes = append(c.Sizes, n)
	}
	return c, nil
}

// Build returns the image that a snapshot of a container started from run
// should publish: run plus the layer in newTar (an uncompressed, already
// filtered layer tar of newBytes bytes). Over the quota it returns ErrQuota; when the chain
// would outgrow pol.MaxLayers it squashes the snapshot layers of run and the new
// one into a single layer. Temporary files go to workDir, which must outlive
// the use of the returned image.
func Build(run v1.Image, newTar string, newBytes int64, pol Policy, workDir string) (v1.Image, Chain, error) {
	chain, err := ChainOf(run)
	if err != nil {
		return nil, Chain{}, err
	}
	if err := CheckQuota(chain.Bytes(), newBytes, pol.WriteQuota); err != nil {
		return nil, chain, err
	}
	layerType, err := snapshotLayerType(run)
	if err != nil {
		return nil, chain, err
	}
	opts := []tarball.LayerOption{tarball.WithMediaType(layerType), tarball.WithCompressedCaching}

	if !NeedSquash(chain.Layers(), pol.MaxLayers) {
		l, err := tarball.LayerFromFile(newTar, opts...)
		if err != nil {
			return nil, chain, err
		}
		img, err := mutate.Append(run, snapshotAddendum(l, newBytes))
		if err != nil {
			return nil, chain, err
		}
		return img, Chain{Base: chain.Base, Sizes: append(append([]int64{}, chain.Sizes...), newBytes)}, nil
	}

	// Squash: merge every snapshot layer of run and the new one.
	layers, err := run.Layers()
	if err != nil {
		return nil, chain, err
	}
	var openers []Opener
	for _, l := range layers[chain.Base:] {
		l := l
		openers = append(openers, func() (io.ReadCloser, error) { return l.Uncompressed() })
	}
	openers = append(openers, func() (io.ReadCloser, error) { return os.Open(newTar) })

	mergedPath := filepath.Join(workDir, "squashed.tar")
	f, err := os.Create(mergedPath)
	if err != nil {
		return nil, chain, err
	}
	st, err := MergeLayers(openers, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, chain, fmt.Errorf("squash: %w", err)
	}
	merged, err := tarball.LayerFromFile(mergedPath, opts...)
	if err != nil {
		return nil, chain, err
	}
	base, err := truncate(run, chain.Base)
	if err != nil {
		return nil, chain, err
	}
	img, err := mutate.Append(base, snapshotAddendum(merged, st.Bytes))
	if err != nil {
		return nil, chain, err
	}
	return img, Chain{Base: chain.Base, Sizes: []int64{st.Bytes}}, nil
}

func snapshotAddendum(l v1.Layer, size int64) mutate.Addendum {
	return mutate.Addendum{
		Layer: l,
		History: v1.History{
			Created:   v1.Time{Time: time.Now().UTC()},
			CreatedBy: historyCreatedBy,
			Comment:   "writable layer snapshot",
		},
		Annotations: map[string]string{AnnotationLayerSize: strconv.FormatInt(size, 10)},
	}
}

// snapshotLayerType picks the media type of a new layer to match the manifest.
func snapshotLayerType(img v1.Image) (types.MediaType, error) {
	mt, err := img.MediaType()
	if err != nil {
		return "", err
	}
	if mt == types.DockerManifestSchema2 {
		return types.DockerLayer, nil
	}
	return types.OCILayer, nil
}

// truncate returns img cut down to its first n layers, config included.
func truncate(img v1.Image, n int) (v1.Image, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg = cfg.DeepCopy()
	cfg.RootFS.DiffIDs = cfg.RootFS.DiffIDs[:n]
	// Drop the history of the removed layers; empty-layer entries are kept.
	var hist []v1.History
	seen := 0
	for _, h := range cfg.History {
		if !h.EmptyLayer {
			if seen >= n {
				continue
			}
			seen++
		}
		hist = append(hist, h)
	}
	cfg.History = hist

	out := empty.Image
	for i := 0; i < n; i++ {
		out, err = mutate.Append(out, mutate.Addendum{Layer: layers[i], Annotations: m.Layers[i].Annotations, MediaType: m.Layers[i].MediaType})
		if err != nil {
			return nil, err
		}
	}
	out, err = mutate.ConfigFile(out, cfg)
	if err != nil {
		return nil, err
	}
	if m.MediaType != "" {
		out = mutate.MediaType(out, m.MediaType)
	}
	if m.Config.MediaType != "" {
		out = mutate.ConfigMediaType(out, m.Config.MediaType)
	}
	return out, nil
}

// ErrNoChange tells that the filtered layer holds nothing.
var ErrNoChange = errors.New("no changes to snapshot")
