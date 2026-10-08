package devicestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/typeurl/v2"
	"github.com/go-logr/logr"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// freezeLimit bounds how long a container stays frozen for one snapshot.
const freezeLimit = 30 * time.Second

// ContainerdRuntime is the Runtime backed by the node's containerd, reached
// through the CRI socket (containerd serves its native API on the same one).
type ContainerdRuntime struct {
	client     *containerd.Client
	namespace  string
	cgroupRoot string
	log        logr.Logger
	seq        atomic.Uint64
	noFreezer  atomic.Bool
}

// NewContainerdRuntime connects to containerd at sock; namespace is the one the
// kubelet's CRI uses ("k8s.io"); cgroupRoot is where the host's cgroup v2 tree is mounted.
func NewContainerdRuntime(sock, namespace, cgroupRoot string, log logr.Logger) (*ContainerdRuntime, error) {
	c, err := containerd.New(sock, containerd.WithDefaultNamespace(namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd %s: %w", sock, err)
	}
	return &ContainerdRuntime{client: c, namespace: namespace, cgroupRoot: cgroupRoot, log: log}, nil
}

// Close releases the connection.
func (r *ContainerdRuntime) Close() error { return r.client.Close() }

func (r *ContainerdRuntime) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, r.namespace)
}

// Inspect implements Runtime.
func (r *ContainerdRuntime) Inspect(ctx context.Context, id string) (Container, error) {
	ctx = r.ctx(ctx)
	cont, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return Container{}, err
	}
	info, err := cont.Info(ctx)
	if err != nil {
		return Container{}, err
	}
	mounts, err := r.client.SnapshotService(info.Snapshotter).Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return Container{}, fmt.Errorf("snapshot mounts of %s: %w", id, err)
	}
	upper := UpperDir(mounts)
	if upper == "" {
		return Container{}, fmt.Errorf("snapshotter %q has no overlay upper directory (state persistence needs the overlayfs snapshotter)", info.Snapshotter)
	}
	c := Container{ID: id, ImageRef: info.Image, UpperDir: upper, Snapshotter: info.Snapshotter, SnapshotKey: info.SnapshotKey}
	if spec, err := cont.Spec(ctx); err == nil && spec.Linux != nil {
		c.Cgroup = CgroupDir(r.cgroupRoot, spec.Linux.CgroupsPath)
		c.IDs = snapshot.IDMaps{UID: idMapOf(spec.Linux.UIDMappings), GID: idMapOf(spec.Linux.GIDMappings)}
	}
	if c.Cgroup == "" && r.cgroupRoot != "" {
		c.Cgroup = FindCgroup(r.cgroupRoot, id)
	}
	return c, nil
}

func idMapOf(in []specs.LinuxIDMapping) snapshot.IDMap {
	m := make(snapshot.IDMap, 0, len(in))
	for _, l := range in {
		m = append(m, snapshot.IDMapping{ContainerID: l.ContainerID, HostID: l.HostID, Size: l.Size})
	}
	return m
}

// UpperDir returns the overlay upperdir of a snapshot's mounts, "" if none.
func UpperDir(mounts []mount.Mount) string {
	for _, m := range mounts {
		for _, o := range m.Options {
			if v, ok := strings.CutPrefix(o, "upperdir="); ok {
				return v
			}
		}
	}
	return ""
}

// Diff implements Runtime: the containerd diff service compares the container's
// active snapshot with its parent, so whiteouts, opaque directories, xattrs and
// ownership come out exactly as an image layer would hold them.
func (r *ContainerdRuntime) Diff(ctx context.Context, c Container, freeze bool) (io.ReadCloser, error) {
	ctx = r.ctx(ctx)
	ctx, done, err := r.client.WithLease(ctx)
	if err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	release := func() { _ = done(context.WithoutCancel(ctx)) }

	sn := r.client.SnapshotService(c.Snapshotter)
	stat, err := sn.Stat(ctx, c.SnapshotKey)
	if err != nil {
		release()
		return nil, fmt.Errorf("stat snapshot %s: %w", c.SnapshotKey, err)
	}
	viewKey := fmt.Sprintf("cybericebox-diff-%s-%d", c.ID, r.seq.Add(1))
	lower, err := sn.View(ctx, viewKey, stat.Parent)
	if err != nil {
		release()
		return nil, fmt.Errorf("view parent snapshot: %w", err)
	}
	removeView := func() { _ = sn.Remove(context.WithoutCancel(ctx), viewKey) }
	upper, err := sn.Mounts(ctx, c.SnapshotKey)
	if err != nil {
		removeView()
		release()
		return nil, fmt.Errorf("mounts of snapshot %s: %w", c.SnapshotKey, err)
	}

	cctx := ctx
	if freeze {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, freezeLimit)
		defer cancel()
		frozenAt := time.Now()
		thaw, ferr := r.freeze(cctx, c)
		if ferr != nil && !errors.Is(ferr, ErrNoFreezer) {
			// A freezer that exists but did not freeze the container in time: the diff of a running container that was not frozen can
			// race with its writes (a file turned into a symlink under the reader), so it is not taken. The snapshot is retried.
			removeView()
			release()
			return nil, fmt.Errorf("the running container could not be frozen, so it is not snapshotted: %w", ferr)
		}
		if ferr == nil {
			defer func() {
				thaw()
				r.log.Info("container frozen for the snapshot", "container", c.ID, "frozen", time.Since(frozenAt).String())
			}()
		}
	}
	desc, err := r.client.DiffService().Compare(cctx, lower, upper, diff.WithMediaType(ocispec.MediaTypeImageLayer))
	removeView()
	if err != nil {
		release()
		return nil, fmt.Errorf("compare snapshot with its parent: %w", err)
	}

	cs := r.client.ContentStore()
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		release()
		return nil, fmt.Errorf("read diff %s: %w", desc.Digest, err)
	}
	return &diffReader{Reader: content.NewReader(ra), close: func() {
		_ = ra.Close()
		// The blob is not deleted here: identical diffs of different devices have the same
		// digest, and deleting it pulled it from under a reader that was still using it
		// ("content digest not found"). It belongs to the lease, and containerd collects
		// it when the last lease that holds it is gone.
		release()
	}}, nil
}

// freeze freezes the container and syncs its filesystem. Without a freezer at all (ErrNoFreezer: an older cgroup layout) the snapshot
// proceeds unfrozen (crash-consistent, as after a power loss); a freezer that fails is an error the caller refuses to go on after.
func (r *ContainerdRuntime) freeze(ctx context.Context, c Container) (func(), error) {
	thaw, err := Freeze(ctx, c.Cgroup)
	if err != nil {
		if errors.Is(err, ErrNoFreezer) {
			if r.noFreezer.CompareAndSwap(false, true) {
				r.log.Info("cgroup v2 freezer not available; snapshots are taken without freezing", "container", c.ID)
			}
		} else {
			r.log.Error(err, "freeze container; snapshotting without freezing", "container", c.ID)
		}
		return nil, err
	}
	if err := syncFilesystem(c.UpperDir); err != nil {
		r.log.Error(err, "sync before snapshot", "upper", c.UpperDir)
	}
	return thaw, nil
}

type diffReader struct {
	io.Reader
	close func()
}

func (d *diffReader) Close() error {
	d.close()
	return nil
}

// LoadImage implements Runtime: the image the kubelet pulled for this container
// is read out of the content store, so no registry is contacted.
func (r *ContainerdRuntime) LoadImage(ctx context.Context, ref string) (v1.Image, error) {
	ctx = r.ctx(ctx)
	img, err := r.client.GetImage(ctx, ref)
	if err != nil {
		return nil, err
	}
	cs := r.client.ContentStore()
	manifest, err := resolveManifest(ctx, cs, img.Target(), platforms.Default())
	if err != nil {
		return nil, err
	}
	h, err := v1.NewHash(manifest.String())
	if err != nil {
		return nil, err
	}
	return snapshot.LoadImage(ctx, contentSource{cs}, h)
}

// resolveManifest finds the manifest of the node's platform below an image
// descriptor (a manifest itself, or an index of manifests).
func resolveManifest(ctx context.Context, cs content.Provider, desc ocispec.Descriptor, m platforms.Matcher) (digest.Digest, error) {
	switch {
	case images.IsManifestType(desc.MediaType):
		return desc.Digest, nil
	case images.IsIndexType(desc.MediaType):
		ra, err := cs.ReaderAt(ctx, desc)
		if err != nil {
			return "", err
		}
		defer func() { _ = ra.Close() }()
		var idx ocispec.Index
		if err := json.NewDecoder(content.NewReader(ra)).Decode(&idx); err != nil {
			return "", fmt.Errorf("parse index %s: %w", desc.Digest, err)
		}
		for _, d := range idx.Manifests {
			if d.Platform != nil && !m.Match(*d.Platform) {
				continue
			}
			if images.IsManifestType(d.MediaType) {
				return d.Digest, nil
			}
		}
	}
	return "", fmt.Errorf("no manifest for this platform under %s", desc.Digest)
}

type contentSource struct{ cs content.Provider }

func (s contentSource) ReadBlob(ctx context.Context, h v1.Hash) (io.ReadCloser, int64, error) {
	ra, err := s.cs.ReaderAt(ctx, ocispec.Descriptor{Digest: digest.Digest(h.String())})
	if err != nil {
		return nil, 0, err
	}
	return struct {
		io.Reader
		io.Closer
	}{content.NewReader(ra), ra}, ra.Size(), nil
}

// Exits implements Runtime: init-process exit events of the CRI namespace. The
// subscription is re-established after a connection error.
func (r *ContainerdRuntime) Exits(ctx context.Context) (<-chan string, error) {
	out := make(chan string, 64)
	filter := fmt.Sprintf(`topic=="/tasks/exit",namespace==%q`, r.namespace)
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			envs, errs := r.client.EventService().Subscribe(ctx, filter)
		stream:
			for {
				select {
				case <-ctx.Done():
					return
				case err := <-errs:
					if err != nil && ctx.Err() == nil {
						r.log.Error(err, "containerd event stream ended; resubscribing")
					}
					break stream
				case env, ok := <-envs:
					if !ok {
						break stream
					}
					v, err := typeurl.UnmarshalAny(env.Event)
					if err != nil {
						continue
					}
					if e, ok := v.(*eventtypes.TaskExit); ok && e.ID == e.ContainerID {
						select {
						case out <- e.ContainerID:
						case <-ctx.Done():
							return
						}
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return out, nil
}

// Quiesce uses the native freezer and treats sync failures as required-capture failures.
// On sync failure the caller still owns the thaw function and must invalidate first.
func (r *ContainerdRuntime) Quiesce(ctx context.Context, c Container) (func(), error) {
	cont, err := r.client.LoadContainer(r.ctx(ctx), c.ID)
	if err != nil {
		return nil, err
	}
	if _, err = cont.Task(r.ctx(ctx), nil); err != nil {
		return nil, err
	}
	thaw, err := Freeze(ctx, c.Cgroup)
	if err != nil {
		return nil, err
	}
	return thaw, syncFilesystem(c.UpperDir)
}
func (r *ContainerdRuntime) TaskAlive(ctx context.Context, c Container) (bool, error) {
	cont, err := r.client.LoadContainer(r.ctx(ctx), c.ID)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	task, err := cont.Task(r.ctx(ctx), nil)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status, err := task.Status(r.ctx(ctx))
	if err != nil {
		return false, err
	}
	return status.Status != containerd.Stopped, nil
}
