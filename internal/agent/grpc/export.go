package grpc

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/snapshot"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// exportChunkSize is the size of the data chunks of an export.
const exportChunkSize = 1 << 20

const exportFormat = "tar+gzip; OCI layer: whiteouts as .wh.<name>, opaque directories as .wh..wh..opq"

// SetRegistryAddr tells the agent where the platform registry (zot) is, as it reaches it
// (host:port). Snapshot export reads the snapshots from there.
func (h *Handler) SetRegistryAddr(addr string) { h.registryAddr = addr }

// ExportDeviceSnapshot streams the latest state of a snapshot-backed device as one
// archive: the squashed difference to its base image. See the proto for the format.
func (h *Handler) ExportDeviceSnapshot(in *protobuf.DeviceSnapshotRequest, stream protobuf.LabManager_ExportDeviceSnapshotServer) error {
	ctx := stream.Context()
	ref := in.GetRef()
	if ref.GetLabGroup() == "" || ref.GetLab() == "" || ref.GetName() == "" {
		return invalid("ref: lab_group, lab and name (the device) are required")
	}
	ns, err := h.newResolver().namespace(ctx, ref.GetLabGroup())
	if err != nil {
		return apiErrorToStatus(err)
	}
	dev, err := h.cs.LaboratoryV1alpha1().Devices(ns).Get(ctx, fmt.Sprintf("%s-%s", crName(ref.GetLab()), ref.GetName()), metav1.GetOptions{})
	if err != nil {
		return apiErrorToStatus(err)
	}
	if err := rejectTerminating(kindDevice, dev); err != nil {
		return err
	}
	if !dev.Spec.StateEnabled() {
		return status.Errorf(codes.FailedPrecondition, "device %s of lab %s has no state persistence: there is no snapshot to export", ref.GetName(), ref.GetLab())
	}
	st := dev.Status.State
	if st == nil || st.Image == "" {
		return status.Errorf(codes.FailedPrecondition, "device %s of lab %s has no snapshot yet (nothing differs from its base image, or the first snapshot is not taken)", ref.GetName(), ref.GetLab())
	}
	if h.registryAddr == "" {
		return status.Error(codes.FailedPrecondition, "the snapshot registry is not configured on the agent")
	}
	repo, digest, err := splitSnapshotRef(st.Image)
	if err != nil {
		return status.Errorf(codes.Internal, "snapshot reference %q: %v", st.Image, err)
	}
	imgRef, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", h.registryAddr, repo, digest), name.Insecure)
	if err != nil {
		return status.Errorf(codes.Internal, "snapshot reference: %v", err)
	}
	img, err := remote.Image(imgRef, remote.WithContext(ctx))
	if err != nil {
		return status.Errorf(codes.Unavailable, "read the snapshot %s from the registry: %v", imgRef.Name(), err)
	}
	meta := &protobuf.SnapshotMeta{
		Ref:             ref,
		BaseImage:       dev.Spec.Image,
		BaseImageDigest: dev.Spec.ImageDigests[dev.Spec.Image],
		SnapshotDigest:  digest,
	}
	if st.SnapshotAt != nil {
		meta.SnapshotUnixMs = st.SnapshotAt.UnixMilli()
	}
	return exportImage(ctx, img, meta, exportChunkSize, stream.Send)
}

// splitSnapshotRef splits "<host>/<repo>@<digest>" into the repository path and the digest.
func splitSnapshotRef(ref string) (repo, digest string, err error) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return "", "", fmt.Errorf("not a digest reference")
	}
	slash := strings.Index(ref, "/")
	if slash < 0 || slash > at {
		return "", "", fmt.Errorf("no repository")
	}
	return ref[slash+1 : at], ref[at+1:], nil
}

// exportImage squashes the snapshot layers of a snapshot image into one tar, compresses it and
// sends meta, the data chunks and the trailer. It is the registry-independent core of the export.
func exportImage(ctx context.Context, img v1.Image, meta *protobuf.SnapshotMeta, chunkSize int, send func(*protobuf.SnapshotChunk) error) error {
	chain, err := snapshot.ChainOf(img)
	if err != nil {
		return status.Errorf(codes.Internal, "read the snapshot chain: %v", err)
	}
	if chain.Layers() == 0 {
		return status.Error(codes.FailedPrecondition, "the snapshot holds no changes to the base image")
	}
	layers, err := img.Layers()
	if err != nil {
		return status.Errorf(codes.Internal, "read the snapshot layers: %v", err)
	}
	if meta.SnapshotDigest == "" {
		if d, err := img.Digest(); err == nil {
			meta.SnapshotDigest = d.String()
		}
	}
	for _, l := range layers[:chain.Base] {
		d, err := l.Digest()
		if err != nil {
			return status.Errorf(codes.Internal, "base layer digest: %v", err)
		}
		meta.BaseLayerDigests = append(meta.BaseLayerDigests, d.String())
	}
	meta.SquashedLayers = int32(chain.Layers())
	meta.LayersSizeBytes = chain.Bytes()
	meta.Format = exportFormat
	meta.ChunkSize = int32(chunkSize)
	if err := send(&protobuf.SnapshotChunk{Content: &protobuf.SnapshotChunk_Meta{Meta: meta}}); err != nil {
		return err
	}

	var openers []snapshot.Opener
	for _, l := range layers[chain.Base:] {
		l := l
		openers = append(openers, func() (io.ReadCloser, error) { return l.Uncompressed() })
	}
	out := &chunkWriter{ctx: ctx, size: chunkSize, send: send, sum: sha256.New()}
	gz, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if _, err := snapshot.MergeLayers(openers, gz); err != nil {
		return status.Errorf(codes.Unavailable, "squash the snapshot layers: %v", err)
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := out.flush(); err != nil {
		return err
	}
	return send(&protobuf.SnapshotChunk{Content: &protobuf.SnapshotChunk_Trailer{Trailer: &protobuf.SnapshotTrailer{
		CompressedBytes: out.total, Sha256: hex.EncodeToString(out.sum.Sum(nil)),
	}}})
}

// chunkWriter cuts what is written into chunks of a fixed size and sends them.
type chunkWriter struct {
	ctx   context.Context
	size  int
	send  func(*protobuf.SnapshotChunk) error
	buf   []byte
	total int64
	sum   interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			return 0, err
		}
		room := w.size - len(w.buf)
		take := len(p)
		if take > room {
			take = room
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		if len(w.buf) == w.size {
			if err := w.flush(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (w *chunkWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	data := w.buf
	w.buf = make([]byte, 0, w.size)
	w.total += int64(len(data))
	_, _ = w.sum.Write(data)
	return w.send(&protobuf.SnapshotChunk{Content: &protobuf.SnapshotChunk_Data{Data: data}})
}
