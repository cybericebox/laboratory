// Package devicestate is the node-agent side of device state persistence: it
// follows the snapshot-backed device containers of a node, detects changes of
// their writable layer, and turns them into snapshot images in the platform
// registry. The container runtime, the cluster API and the registry are
// interfaces, so the logic runs (and is tested) without containerd.
package devicestate

import (
	"context"
	"io"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// PodInfo is what the engine needs to know about one snapshot-backed device
// pod on this node.
type PodInfo struct {
	// Device is the Device CR the pod belongs to.
	Device types.NamespacedName
	Pod    string
	// Incarnation is the pod's incarnation number; only the current incarnation
	// of a device records snapshots.
	Incarnation int32
	// ContainerID is the runtime id of the device container; empty until started.
	ContainerID string
	Running     bool
	// Ended is true once the pod finished (container exited).
	Ended bool
	// Epoch is the reset epoch the pod was created in; DeviceEpoch is the
	// device's current one. A pod of an older epoch is never snapshotted.
	Epoch       int32
	DeviceEpoch int32
	// ExitDone is true when the exit snapshot of this pod is already finished.
	ExitDone bool
	Policy   snapshot.Policy
	Repo     string
	// Tenant is the tenant the device belongs to (names.DefaultTenant when the device carries no tenant label);
	// TenantQuota is the most all of that tenant's snapshots may take in the registry together (0 = no limit).
	Tenant      string
	TenantQuota int64
}

// Container is the runtime view of a device container.
type Container struct {
	ID string
	// ImageRef is the image the container was created from.
	ImageRef string
	// UpperDir is the writable layer directory on the node.
	UpperDir string
	// Cgroup is the container's cgroup directory (cgroup v2), "" if unknown.
	Cgroup string
	// Snapshotter and SnapshotKey name the container's writable snapshot in the
	// runtime; they are private to the Runtime implementation.
	Snapshotter string
	SnapshotKey string
	// IDs are the user and group id maps of the container's user namespace (empty without one): the diff of its writable layer
	// holds host ids, which a snapshot must not keep.
	IDs snapshot.IDMaps
}

// Runtime is the container runtime facade.
type Runtime interface {
	// Inspect finds a container by id.
	Inspect(ctx context.Context, containerID string) (Container, error)
	// Diff returns the uncompressed layer tar of the container's writable layer
	// against its parent snapshot. With freeze the container is frozen and its
	// filesystem synced while the diff is computed, so the result is consistent;
	// without it (the container has exited) nothing runs and nothing is frozen.
	// Closing the reader releases everything the diff held.
	Diff(ctx context.Context, c Container, freeze bool) (io.ReadCloser, error)
	// LoadImage opens the image a container was created from out of the local
	// image store, with the snapshot chain already on the node.
	LoadImage(ctx context.Context, imageRef string) (v1.Image, error)
	// Exits reports the ids of containers whose task exited, until ctx ends.
	Exits(ctx context.Context) (<-chan string, error)
}

// Snapshot is the outcome of a successful snapshot.
type Snapshot struct {
	// Image is the pullable reference with digest; empty when the writable
	// layer holds nothing beyond the image the device started from and that
	// image is the base image.
	Image     string
	At        time.Time
	SizeBytes int64
	Layers    int32
}

// Cluster is the Kubernetes side: which pods to follow and where to report.
// Every write is guarded by the reset epoch: it is dropped (ErrStale) when the
// device has been reset since the pod was created.
type Cluster interface {
	// Pods lists the snapshot-backed device pods scheduled on this node.
	Pods(ctx context.Context) ([]PodInfo, error)
	// Record stores a successful snapshot and clears the warning.
	Record(ctx context.Context, p PodInfo, s Snapshot) error
	// Warn stores a warning and keeps the last good snapshot.
	Warn(ctx context.Context, p PodInfo, msg string) error
	// MarkExit records that the exit snapshot of the pod is finished.
	MarkExit(ctx context.Context, p PodInfo) error
	// TenantBytes is what the snapshots of the tenant's devices take in the registry (the sizes recorded in their status),
	// without the device `except`.
	TenantBytes(ctx context.Context, tenant string, except types.NamespacedName) (int64, error)
	// LiveRepos is the set of snapshot repositories of the devices that exist (every tenant): any other repository of the
	// registry belongs to a lab that is gone and is only waiting out its retention.
	LiveRepos(ctx context.Context) (map[string]bool, error)
}

// Superseder removes what a new snapshot replaced; *snapshot.Registry implements it. A Pusher that does not is never asked.
type Superseder interface {
	Supersede(ctx context.Context, repo, oldRef string, keep v1.Image) error
}

// SpaceGuard says whether the registry has room for a push of about this many bytes; *snapshot.Capacity implements it.
type SpaceGuard interface {
	Check(ctx context.Context, bytes int64) error
}

// RetainedCounter is the state a tenant keeps in the repositories of labs that are gone; *snapshot.Registry implements it.
type RetainedCounter interface {
	RetainedBytes(ctx context.Context, tenant string, live map[string]bool) (int64, error)
}

// Pusher publishes snapshot images; *snapshot.Registry implements it.
type Pusher interface {
	Push(ctx context.Context, repo string, img v1.Image, baseLayers int, sourceRepo string) (ref string, digest v1.Hash, err error)
}
