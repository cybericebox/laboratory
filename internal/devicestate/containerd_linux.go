package devicestate

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	"github.com/google/go-containerregistry/pkg/authn"
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
	// SourceKeychain binds fallback reads to the current pod's pull credentials.
	SourceKeychain func(context.Context, Container, PodInfo) (authn.Keychain, error)
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
	inspectContainerSpec(ctx, &c, cont.Spec, r.cgroupRoot)
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
// active snapshot with its parent. Containerd provides whiteouts, opaque markers
// and security.capability; rooted metadata reads preserve user.* attributes and
// supplement attribute-only changes. Directory attribute removals fail explicitly
// because containerd's directory merge cannot restore them faithfully.
func (r *ContainerdRuntime) Diff(ctx context.Context, c Container, freeze bool, pol snapshot.Policy) (io.ReadCloser, error) {
	ctx = r.ctx(ctx)
	ctx, done, err := r.client.WithLease(ctx)
	if err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	sn := r.client.SnapshotService(c.Snapshotter)
	cctx, cancel := context.WithCancel(ctx)
	var ownedView string
	var thaw func()
	var closeContent func() error
	cleanup := func() {
		// Ordinary quiescence must not wait on content/RPC cleanup. Required
		// capture owns a separate guard and has no thaw callback here.
		if thaw != nil {
			thaw()
		}
		if closeContent != nil {
			_ = closeContent()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cleanupCancel()
		if ownedView != "" {
			_ = sn.Remove(cleanupCtx, ownedView)
		}
		_ = done(cleanupCtx)
		cancel()
	}
	stat, err := sn.Stat(ctx, c.SnapshotKey)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("stat snapshot %s: %w", c.SnapshotKey, err)
	}
	viewKey := r.diffViewKey(c.ID)
	lower, err := sn.View(ctx, viewKey, stat.Parent)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("view parent snapshot: %w", err)
	}
	ownedView = viewKey
	upper, err := sn.Mounts(ctx, c.SnapshotKey)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("mounts of snapshot %s: %w", c.SnapshotKey, err)
	}

	if freeze {
		cancel()
		cctx, cancel = diffFreezeContext(ctx)
		frozenAt := time.Now()
		unfreeze, ferr := r.freeze(cctx, c)
		if ferr != nil && !errors.Is(ferr, ErrNoFreezer) {
			// A freezer that exists but did not freeze the container in time: the diff of a running container that was not frozen can
			// race with its writes (a file turned into a symlink under the reader), so it is not taken. The snapshot is retried.
			cleanup()
			return nil, fmt.Errorf("the running container could not be frozen, so it is not snapshotted: %w", ferr)
		}
		if ferr == nil {
			thaw = func() {
				unfreeze()
				r.log.Info("container frozen for the snapshot", "container", c.ID, "frozen", time.Since(frozenAt).String())
			}
		}
	}
	desc, err := r.client.DiffService().Compare(cctx, lower, upper, diff.WithMediaType(ocispec.MediaTypeImageLayer))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("compare snapshot with its parent: %w", err)
	}

	cs := r.client.ContentStore()
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("read diff %s: %w", desc.Digest, err)
	}
	closeContent = ra.Close
	return managedDiffReader(cctx, cancel, func(out io.Writer) error {
		// Removing ID-map options makes ownership match the host ids supplied by
		// the raw diff. The filter is the sole owner-id translation boundary.
		return mount.WithReadonlyTempMount(cctx, mount.RemoveIDMapOption(lower), func(lowerPath string) error {
			return mount.WithReadonlyTempMount(cctx, mount.RemoveIDMapOption(upper), func(livePath string) error {
				up, err := os.OpenRoot(c.UpperDir)
				if err != nil {
					return err
				}
				defer up.Close()
				live, err := os.OpenRoot(livePath)
				if err != nil {
					return err
				}
				defer live.Close()
				lo, err := os.OpenRoot(lowerPath)
				if err != nil {
					return err
				}
				defer lo.Close()
				return writeUserXattrLayer(cctx, content.NewReader(ra), out, up, live, lo, pol)
			})
		})
	}, cleanup), nil // no shared content deletion; containerd owns garbage collection
}

func (r *ContainerdRuntime) diffViewKey(containerID string) string {
	// A crashed process may leave its leased view behind. A nonce prevents a
	// fresh process (whose sequence starts over) from colliding with that view,
	// including callers that already carry a lease in their context.
	return fmt.Sprintf("cybericebox-diff-%s-%d-%s", containerID, r.seq.Add(1), rand.Text())
}

func diffFreezeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	// The reader/cleanup owns cancellation after Diff returns.
	return context.WithTimeout(ctx, freezeLimit)
}

// managedDiffReader cancels a blocked producer even when a consumer abandons
// the stream without Close. Cleanup runs once and finishes before EOF/Close is
// reported, so ordinary snapshots do not retain a freezer through upload.
func managedDiffReader(ctx context.Context, cancel context.CancelFunc, produce func(io.Writer) error, cleanup func()) io.ReadCloser {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	var producerErr error
	stopCancellation := context.AfterFunc(ctx, func() { _ = pw.CloseWithError(ctx.Err()) })
	go func() {
		defer close(done)
		producerErr = produce(pw)
		stopCancellation()
		cleanup()
		_ = pw.CloseWithError(producerErr)
	}()
	return &diffReader{Reader: pr, close: func() error {
		_ = pr.Close()
		cancel()
		<-done
		return producerErr
	}}
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
	close func() error
}

func (d *diffReader) Close() error {
	return d.close()
}

// LoadImage implements Runtime: the image the kubelet pulled for this container
// is read out of the content store, so no registry is contacted.
func (r *ContainerdRuntime) LoadImage(ctx context.Context, ref string) (v1.Image, error) {
	return r.loadImage(ctx, ref, nil)
}

func (r *ContainerdRuntime) LoadImageForPod(ctx context.Context, c Container, p PodInfo) (v1.Image, error) {
	if r.SourceKeychain == nil {
		return nil, fmt.Errorf("pod-owned source image credentials are unavailable")
	}
	keychain, err := r.SourceKeychain(ctx, c, p)
	if err != nil {
		return nil, err
	}
	return r.loadImage(ctx, c.ImageRef, keychain)
}

func (r *ContainerdRuntime) loadImage(ctx context.Context, ref string, keychain authn.Keychain) (v1.Image, error) {
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
	return loadRegistryBackedImage(ctx, cs, h, ref, keychain)
}

// resolveManifest finds the manifest of the node's platform below an image
// descriptor (a manifest itself, or an index of manifests).
func resolveManifest(ctx context.Context, cs content.Provider, desc ocispec.Descriptor, m platforms.MatchComparer) (digest.Digest, error) {
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
		var preferred *ocispec.Descriptor
		for i := range idx.Manifests {
			d := &idx.Manifests[i]
			if !images.IsManifestType(d.MediaType) {
				continue
			}
			if d.Platform != nil && !m.Match(*d.Platform) {
				continue
			}
			// Compatibility is not preference: an arm64 node also matches
			// arm variants whose content the kubelet need not have pulled.
			if preferred == nil || (preferred.Platform == nil && d.Platform != nil) ||
				(d.Platform != nil && preferred.Platform != nil && m.Less(*d.Platform, *preferred.Platform)) {
				preferred = d
			}
		}
		if preferred != nil {
			return preferred.Digest, nil
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
func (r *ContainerdRuntime) Quiesce(ctx context.Context, c Container) (func() error, error) {
	cont, err := r.client.LoadContainer(r.ctx(ctx), c.ID)
	if err != nil {
		return nil, err
	}
	if _, err = cont.Task(r.ctx(ctx), nil); err != nil {
		return nil, err
	}
	thaw, err := FreezeRequired(ctx, c.Cgroup)
	if err != nil {
		return thaw, err
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

// Thaw verifies the task's current cgroup identity; callers own API fencing.
func (r *ContainerdRuntime) Thaw(ctx context.Context, c Container) error {
	current, err := r.Inspect(ctx, c.ID)
	if err != nil {
		return err
	}
	if current.Cgroup == "" || current.Cgroup != c.Cgroup {
		return ErrStale
	}
	return os.WriteFile(filepath.Join(current.Cgroup, "cgroup.freeze"), []byte("0"), 0644)
}

// inspectContainerSpec keeps legacy best-effort inspection but explicitly marks
// unknown ownership metadata, so required capture cannot treat it as identity.
func inspectContainerSpec(ctx context.Context, c *Container, read func(context.Context) (*specs.Spec, error), root string) {
	c.OwnershipKnown = false
	c.IDs = snapshot.IDMaps{}
	spec, err := read(ctx)
	if err != nil || spec == nil || spec.Linux == nil {
		return
	}
	c.Cgroup = CgroupDir(root, spec.Linux.CgroupsPath)
	userns := false
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.UserNamespace {
			userns = true
		}
	}
	uid, gid := spec.Linux.UIDMappings, spec.Linux.GIDMappings
	if userns && (len(uid) == 0 || len(gid) == 0) || ((len(uid) > 0) != (len(gid) > 0)) {
		return
	}
	if !validIDMappings(uid) || !validIDMappings(gid) {
		return
	}
	c.IDs = snapshot.IDMaps{UID: idMapOf(uid), GID: idMapOf(gid)}
	c.OwnershipKnown = true
}
func validIDMappings(m []specs.LinuxIDMapping) bool {
	for i, a := range m {
		if a.Size == 0 || uint64(a.ContainerID)+uint64(a.Size) > 1<<32 || uint64(a.HostID)+uint64(a.Size) > 1<<32 {
			return false
		}
		for _, b := range m[:i] {
			if uint64(a.ContainerID) < uint64(b.ContainerID)+uint64(b.Size) && uint64(b.ContainerID) < uint64(a.ContainerID)+uint64(a.Size) || uint64(a.HostID) < uint64(b.HostID)+uint64(b.Size) && uint64(b.HostID) < uint64(a.HostID)+uint64(a.Size) {
				return false
			}
		}
	}
	return true
}
