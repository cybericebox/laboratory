package devicestate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// KubeCluster is the Cluster backed by the Kubernetes API: the device pods of
// this node are found through their annotations and the Device CR carries the
// policy and receives the snapshot status.
type KubeCluster struct {
	// Client reads from the informer cache.
	Client client.Client
	// Reader reads straight from the API server; every status write re-reads
	// through it so the epoch guard never acts on a stale cache.
	Reader   client.Reader
	NodeName string
}

// Pods implements Cluster.
func (k *KubeCluster) Pods(ctx context.Context) ([]PodInfo, error) {
	return k.pods(ctx, k.Client)
}

// CapturePods bypasses the informer cache during crash recovery.
func (k *KubeCluster) CapturePods(ctx context.Context) ([]PodInfo, error) {
	return k.pods(ctx, k.Reader)
}

func (k *KubeCluster) pods(ctx context.Context, reader client.Reader) ([]PodInfo, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.MatchingFields{"spec.nodeName": k.NodeName}); err != nil {
		return nil, err
	}
	var out []PodInfo
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != k.NodeName || p.Annotations[names.AnnotationStateDevice] == "" {
			continue
		}
		var dev laboratoryv1alpha1.Device
		key := types.NamespacedName{Namespace: p.Namespace, Name: p.Annotations[names.AnnotationStateDevice]}
		if err := reader.Get(ctx, key, &dev); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if !dev.Spec.StateEnabled() || dev.Status.State == nil {
			continue
		}
		out = append(out, podInfo(p, &dev))
	}
	return out, nil
}

func podInfo(p *corev1.Pod, dev *laboratoryv1alpha1.Device) PodInfo {
	spec, st := dev.Spec.State, dev.Status.State
	info := PodInfo{
		Device: types.NamespacedName{Namespace: dev.Namespace, Name: dev.Name},
		Pod:    p.Name,
		UID:    string(p.UID), ResourceVersion: p.ResourceVersion, CaptureRequest: spec.CaptureRequest, Capture: st.Capture, Guard: p.Annotations[CaptureGuardAnnotation], Deleting: p.DeletionTimestamp != nil,
		Incarnation: annotationInt(p, names.AnnotationStateIncarnation),
		Epoch:       annotationInt(p, names.AnnotationStateEpoch),
		DeviceEpoch: st.Epoch,
		ExitDone:    st.ExitSnapshotPod == p.Name,
		Running:     p.Status.Phase == corev1.PodRunning,
		Ended:       p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed,
		Policy:      snapshot.NewPolicy(spec.Debounce.Duration, spec.ExcludePaths, spec.WriteQuotaBytes, int(spec.MaxLayers)).WithMaxFileSize(spec.MaxFileBytes).WithMaxEntries(int(spec.MaxEntries)),
		Tenant:      names.TenantOf(dev.Labels),
		TenantQuota: spec.TenantQuotaBytes,
		Repo:        snapshot.Repo(dev.Namespace, dev.Spec.LabRef, dev.Spec.Name),
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == dev.Spec.Name {
			info.ContainerID = containerID(cs.ContainerID)
		}
	}
	return info
}

// containerID strips the runtime scheme: "containerd://abc" is "abc".
func containerID(s string) string {
	if _, id, ok := strings.Cut(s, "://"); ok {
		return id
	}
	return s
}

func annotationInt(p *corev1.Pod, key string) int32 {
	n, _ := strconv.ParseInt(p.Annotations[key], 10, 32)
	return int32(n)
}

// Record implements Cluster.
func (k *KubeCluster) Record(ctx context.Context, p PodInfo, s Snapshot) error {
	return k.update(ctx, p, true, func(st *laboratoryv1alpha1.DeviceStateStatus) {
		st.Image = s.Image
		at := metav1.NewTime(s.At)
		st.SnapshotAt = &at
		st.SizeBytes = s.SizeBytes
		st.Layers = s.Layers
		st.Warning = ""
	})
}

// Warn implements Cluster.
func (k *KubeCluster) Warn(ctx context.Context, p PodInfo, msg string) error {
	return k.update(ctx, p, true, func(st *laboratoryv1alpha1.DeviceStateStatus) { st.Warning = msg })
}

// MarkExit implements Cluster.
func (k *KubeCluster) MarkExit(ctx context.Context, p PodInfo) error {
	return k.update(ctx, p, false, func(st *laboratoryv1alpha1.DeviceStateStatus) { st.ExitSnapshotPod = p.Pod })
}

// update patches status.state of the device with an optimistic lock, retrying on
// conflict. It refuses (ErrStale) when the pod no longer belongs to the device's
// current epoch — and, for snapshot data, current incarnation.
func (k *KubeCluster) update(ctx context.Context, p PodInfo, needCurrent bool, mutate func(*laboratoryv1alpha1.DeviceStateStatus)) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var dev laboratoryv1alpha1.Device
		if err = k.Reader.Get(ctx, p.Device, &dev); err != nil {
			if errors.IsNotFound(err) {
				return ErrStale
			}
			return err
		}
		st := dev.Status.State
		if st == nil || st.Epoch != p.Epoch || (needCurrent && st.Incarnation != p.Incarnation) {
			return ErrStale
		}
		orig := dev.DeepCopy()
		mutate(dev.Status.State)
		err = k.Client.Status().Patch(ctx, &dev, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return fmt.Errorf("patch device %s: %w", p.Device, err)
		}
	}
	return err
}

// TenantBytes implements Cluster: the sum of the snapshot sizes of the tenant's devices. The default tenant also owns the
// devices that carry no tenant label (created before tenancy).
func (k *KubeCluster) TenantBytes(ctx context.Context, tenant string, except types.NamespacedName) (int64, error) {
	var devices laboratoryv1alpha1.DeviceList
	if err := k.Client.List(ctx, &devices, client.MatchingLabels{names.LabelTenant: tenant}); err != nil {
		return 0, err
	}
	items := devices.Items
	if tenant == names.DefaultTenant {
		var legacy laboratoryv1alpha1.DeviceList
		sel, _ := labels.Parse("!" + names.LabelTenant)
		if err := k.Client.List(ctx, &legacy, client.MatchingLabelsSelector{Selector: sel}); err != nil {
			return 0, err
		}
		items = append(items, legacy.Items...)
	}
	var total int64
	for i := range items {
		d := &items[i]
		if d.Namespace == except.Namespace && d.Name == except.Name {
			continue
		}
		if d.Status.State != nil {
			total += d.Status.State.SizeBytes
		}
	}
	return total, nil
}

// LiveRepos implements Cluster: the snapshot repositories of every existing device.
func (k *KubeCluster) LiveRepos(ctx context.Context) (map[string]bool, error) {
	var devices laboratoryv1alpha1.DeviceList
	if err := k.Client.List(ctx, &devices); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(devices.Items))
	for i := range devices.Items {
		d := &devices.Items[i]
		live[snapshot.Repo(d.Namespace, d.Spec.LabRef, d.Spec.Name)] = true
	}
	return live, nil
}
